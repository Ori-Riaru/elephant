package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/abenz1267/elephant/v2/internal/comm/handlers"
	"github.com/abenz1267/elephant/v2/pkg/common"
	"github.com/abenz1267/elephant/v2/pkg/common/history"
	"github.com/abenz1267/elephant/v2/pkg/pb/pb"
	"github.com/tidwall/gjson"
)

var (
	currentSuggestions      = []Suggestion{}
	currentSuggestionsMutex = &sync.RWMutex{}
	pendingCancel           context.CancelFunc
	httpClient              = &http.Client{
		Transport: &http.Transport{
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     5 * time.Second,
		},
	}

	currentBrowserHistory = []*pb.QueryResponse_Item{}
	browserHistoryMutex   = &sync.RWMutex{}  // TODO git rid of mutex for browser history
	browserHistoryCancel  context.CancelFunc // TODO do this is a less advanced way
	browserHistoryDB      *sql.DB
)

func reverseHost(host string) string {
	runes := []rune(host)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func createBrowserHistoryPlaceholders(_ /* queriedEngines */ []Engine, prefix string, query string) []*pb.QueryResponse_Item {
	entries := []*pb.QueryResponse_Item{}

	if config.BrowserProfilePath == "" {
		return entries
	}

	if query == "" {
		return entries
	}

	if prefix == config.EngineFinderPrefix {
		return entries
	}

	browserHistoryMutex.RLock()

	if len(currentBrowserHistory) == 0 {
		browserHistoryMutex.RUnlock()
		return entries
	}

	for slot := 0; slot < len(currentBrowserHistory); slot++ {
		h := currentBrowserHistory[slot]
		placeholderItem := &pb.QueryResponse_Item{
			Identifier: h.Identifier,
			Text:       h.Text,
			Subtext:    h.Subtext,
			Icon:       h.Icon,
			Provider:   Name,
			Score:      h.Score,
			Type:       0,
			State:      []string{"placeholder"},
			Actions:    h.Actions,
		}
		entries = append(entries, placeholderItem)
	}

	browserHistoryMutex.RUnlock()
	return entries
}

func scheduleBrowserHistoryAsync(query string, engines []Engine, filterByHost bool, conn net.Conn, format uint8) {
	if query == "" {
		return
	}

	// Cancel previous pending query
	if browserHistoryCancel != nil {
		browserHistoryCancel()
	}

	// Create context for this query
	ctx, cancel := context.WithCancel(context.Background())
	browserHistoryCancel = cancel

	// Create debounce timer
	time.AfterFunc(time.Duration(config.SuggestionsDebounce)*time.Millisecond, func() {
		select {
		case <-ctx.Done():
			return
		default:
			fetchAndSendBrowserHistory(ctx, query, engines, filterByHost, conn, format)
		}
	})
}

func getBrowserSuggestions(query string, engines []Engine, filterByHost bool) []*pb.QueryResponse_Item {
	entries := []*pb.QueryResponse_Item{}

	if browserHistoryDB == nil {
		slog.Debug(Name, "browser_history", "db is nil")
		return entries
	}

	if len(engines) == 0 {
		return entries
	}

	tokens := strings.Fields(strings.ToLower(query))
	if len(tokens) == 0 {
		return entries
	}

	var conditions []string
	var args []interface{}

	for _, token := range tokens {
		conditions = append(conditions, "(LOWER(h.title) LIKE ? OR LOWER(h.url) LIKE ?)")
		pattern := "%" + token + "%"
		args = append(args, pattern, pattern)
	}
	whereClause := strings.Join(conditions, " AND ")

	var sqlQuery string

	if filterByHost {
		var hostPatterns []string
		for _, engine := range engines {
			u, err := url.Parse(engine.URL)
			if err != nil {
				continue
			}
			if u.Host == "" {
				continue
			}

			host := strings.ToLower(u.Host)
			revHost := reverseHost(host) + "."
			hostPatterns = append(hostPatterns, revHost)
		}

		if len(hostPatterns) == 0 {
			return entries
		}

		hostConditions := make([]string, len(hostPatterns))
		for i, host := range hostPatterns {
			hostConditions[i] = "LOWER(h.rev_host) LIKE ?"
			args = append(args, host+"%")
		}
		hostClause := strings.Join(hostConditions, " OR ")

		sqlQuery = fmt.Sprintf(`
			SELECT
				h.url,
				h.title
			FROM moz_places h
			WHERE %s
			AND (%s)
			AND h.title IS NOT NULL
			AND h.hidden = 0
			ORDER BY h.frecency DESC
			LIMIT ?
		`, whereClause, hostClause)
	} else {
		sqlQuery = fmt.Sprintf(`
			SELECT 
				h.url,
				h.title
			FROM moz_places h
			WHERE %s
			AND h.title IS NOT NULL
			AND h.hidden = 0
			ORDER BY h.frecency DESC
			LIMIT ?
		`, whereClause)
	}

	args = append(args, config.MaxBrowserItems)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	rows, err := browserHistoryDB.QueryContext(ctx, sqlQuery, args...)
	if err != nil {
		slog.Warn(Name, "browser_history", "query failed", "query", query, "error", err)
		return entries
	}
	defer rows.Close()

	i := 0
	for rows.Next() {
		var url, title string
		err := rows.Scan(&url, &title)
		if err != nil {
			slog.Warn(Name, "browser_history", "scan failed", "error", err)
			continue
		}

		entries = append(entries, &pb.QueryResponse_Item{
			Identifier: url,
			Text:       title,
			Subtext:    url,
			Actions:    []string{ActionOpenURL},
			Icon:       Icon(),
			Provider:   Name,
			Score:      int32(config.MaxBrowserItems - i),
			Type:       0,
		})
		i += 1
	}

	slog.Debug(Name, "browser_history", "results", "count", len(entries), "query", query)

	return entries
}

func fetchAndSendBrowserHistory(ctx context.Context, query string, engines []Engine, filterByHost bool, conn net.Conn, format uint8) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	entries := getBrowserSuggestions(query, engines, filterByHost)

	select {
	case <-ctx.Done():
		return
	default:
	}

	browserHistoryMutex.Lock()

	oldEntries := currentBrowserHistory
	currentBrowserHistory = entries

	if len(entries) == 0 {
		for slot := 0; slot < len(oldEntries); slot++ {
			deleteItem := &pb.QueryResponse_Item{
				Identifier: oldEntries[slot].Identifier,
				Text:       "%DELETE%",
			}
			handlers.UpdateItem(format, query, conn, deleteItem)
		}
		browserHistoryMutex.Unlock()
		return
	}

	for i, item := range entries {
		updatedItem := &pb.QueryResponse_Item{
			Identifier: item.Identifier,
			Text:       item.Text,
			Subtext:    item.Subtext,
			Actions:    item.Actions,
			Icon:       item.Icon,
			Provider:   Name,
			Score:      int32(config.MaxBrowserItems - i),
			Type:       0,
		}
		handlers.UpdateItem(format, query, conn, updatedItem)
	}

	for i := len(entries); i < len(oldEntries); i++ {
		deleteItem := &pb.QueryResponse_Item{
			Identifier: oldEntries[i].Identifier,
			Text:       "%DELETE%",
		}
		handlers.UpdateItem(format, query, conn, deleteItem)
	}

	browserHistoryMutex.Unlock()

	browserHistoryCancel = nil
}

func scheduleSuggestionsAsync(queriedEngines []Engine, prefix string, query string, conn net.Conn, format uint8) {
	// Check if any engine has suggestions URL and query is not empty
	shouldFetch := false
	for _, engine := range queriedEngines {
		if engine.SuggestionsURL != "" && query != "" {
			shouldFetch = true
			break
		}
	}

	if !shouldFetch {
		return
	}

	// Cancel previous pending query
	if pendingCancel != nil {
		pendingCancel()
	}

	// Create context for this query
	ctx, cancel := context.WithCancel(context.Background())
	pendingCancel = cancel

	// Create debounce timer
	time.AfterFunc(time.Duration(config.SuggestionsDebounce)*time.Millisecond, func() {
		// Only execute if this query hasn't been cancelled
		select {
		case <-ctx.Done():
			// Query was cancelled, don't fetch
			return
		default:
			fetchAndSendSuggestions(ctx, queriedEngines, prefix, query, conn, format)
		}
	})
}

func createPlaceholderSlots(queriedEngines []Engine, _ /* prefix */ string, query string) []*pb.QueryResponse_Item {
	entries := []*pb.QueryResponse_Item{}

	hasSuggestions := false
	for _, engine := range queriedEngines {
		if engine.SuggestionsURL != "" && query != "" {
			hasSuggestions = true
			break
		}
	}

	if !hasSuggestions {
		return entries
	}

	queriedEnginesSet := make(map[string]bool)
	for _, engine := range queriedEngines {
		queriedEnginesSet[engine.Identifier] = true
	}

	currentSuggestionsMutex.RLock()
	for slot := 0; slot < config.MaxApiItems; slot++ {
		placeholderEntry := &pb.QueryResponse_Item{
			Identifier: fmt.Sprintf("websearch-slot-%d", slot),
			Text:       "",
			Subtext:    "",
			Icon:       "",
			Provider:   Name,
			Score:      int32(-slot),
			Type:       0,
			State:      []string{"placeholder"},
		}

		if slot < len(currentSuggestions) { // FIXME dubious if needed
			s := currentSuggestions[slot]

			if queriedEnginesSet[s.Engine.Identifier] {
				placeholderEntry.Text = s.Content
				placeholderEntry.Subtext = s.Engine.Name
				placeholderEntry.Icon = s.Engine.Icon
				placeholderEntry.Identifier = s.Identifier
			}
		}

		entries = append(entries, placeholderEntry)
	}
	currentSuggestionsMutex.RUnlock()

	return entries
}

func fetchApiSuggestions(address string, query string, jsonPath string) ([]string, error) {
	request := expandSubstitutions(address, query)

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(config.SuggestionsTimeout)*time.Millisecond)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", request, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64; rv:133.0) Gecko/20100101 Firefox/133.0")

	res, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to make request: %w", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", res.StatusCode)
	}

	body, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	result := gjson.Get(string(body), jsonPath)

	var suggestions []string
	if result.IsArray() {
		for _, item := range result.Array() {
			suggestions = append(suggestions, item.String())
		}
	} else {
		suggestions = append(suggestions, result.String())
	}

	return suggestions, nil
}

func fetchAndSendSuggestions(ctx context.Context, queriedEngines []Engine, _ /* prefix */ string, query string, conn net.Conn, format uint8) {
	// Check if context was cancelled (new query started)
	select {
	case <-ctx.Done():
		return // Query was cancelled, don't send updates
	default:
	}

	// Fetch Suggestions
	allSuggestions := []Suggestion{}
	var (
		wg sync.WaitGroup
		mu sync.Mutex
	)
	for engineIndex, engine := range queriedEngines {
		if query == "" || engine.SuggestionsURL == "" {
			continue
		}

		wg.Go(func() {
			suggestions, err := fetchApiSuggestions(
				engine.SuggestionsURL,
				query,
				engine.SuggestionsPath,
			)
			if err != nil {
				slog.Warn( // TODO Look in to this
					"fetchSuggestions failed",
					"name", Name,
					slog.Any("error", err),
				)
				return
			}

			engineSuggestions := make([]Suggestion, 0, len(suggestions))
			for i, content := range suggestions {
				engineSuggestions = append(engineSuggestions, Suggestion{
					Identifier: "",
					Content:    content,
					Engine:     engine,
					Score:      int32(-i - engineIndex),
				})
			}

			mu.Lock()
			allSuggestions = append(allSuggestions, engineSuggestions...)
			mu.Unlock()
		})
	}
	wg.Wait()

	// Check if context was cancelled again after fetching
	select {
	case <-ctx.Done():
		return // Query was cancelled, don't send updates
	default:
	}

	// Deduplicate and sort
	sort.Slice(allSuggestions, func(i, j int) bool {
		return allSuggestions[i].Score > allSuggestions[j].Score
	})

	seenSuggestions := make(map[string]bool)
	seenSuggestions[strings.ToLower(strings.TrimSpace(query))] = true

	currentSuggestionsMutex.Lock()

	// Build new suggestions list (limited to MaxApiItems)
	newSuggestions := []Suggestion{}
	suggestionIndex := 0
	for suggestionIndex < len(allSuggestions) && len(newSuggestions) < config.MaxApiItems {
		s := allSuggestions[suggestionIndex]
		normalized := strings.ToLower(strings.TrimSpace(s.Content))

		if !seenSuggestions[normalized] {
			s.Identifier = fmt.Sprintf("websearch-slot-%d", len(newSuggestions))
			newSuggestions = append(newSuggestions, s)
			seenSuggestions[normalized] = true
		}
		suggestionIndex++
	}

	// If no results, delete all placeholders and keep old currentSuggestions
	if len(newSuggestions) == 0 {
		for slot := 0; slot < len(currentSuggestions); slot++ {
			deleteItem := &pb.QueryResponse_Item{
				Identifier: currentSuggestions[slot].Identifier,
				Text:       "%DELETE%",
			}
			handlers.UpdateItem(format, query, conn, deleteItem)
		}
		currentSuggestionsMutex.Unlock()
		return
	}

	// Replace currentSuggestions with new results
	oldSuggestions := currentSuggestions
	currentSuggestions = newSuggestions

	// Send updates for new suggestions
	for i := 0; i < len(currentSuggestions); i++ {
		s := currentSuggestions[i]

		updatedItem := &pb.QueryResponse_Item{
			Identifier: s.Identifier,
			Text:       s.Content,
			Subtext:    s.Engine.Name,
			Icon:       s.Engine.Icon,
			Provider:   Name,
			Score:      int32(-i),
			Type:       0,
			Actions:    []string{ActionSearchSuggestion},
		}

		handlers.UpdateItem(format, query, conn, updatedItem)
	}

	// Delete old placeholder slots that are no longer needed
	for i := len(currentSuggestions); i < len(oldSuggestions); i++ {
		deleteItem := &pb.QueryResponse_Item{
			Identifier: oldSuggestions[i].Identifier,
			Text:       "%DELETE%",
		}
		handlers.UpdateItem(format, query, conn, deleteItem)
	}

	currentSuggestionsMutex.Unlock()

	// Clean up pending cancel
	pendingCancel = nil
}

// TODO: remove unused
func listEngines(query string, _ bool, exact bool) []*pb.QueryResponse_Item {

	entries := []*pb.QueryResponse_Item{}

	for k, v := range config.Engines {
		text := v.Name
		if v.Prefix != "" {
			text = fmt.Sprintf("%s ( %s )", v.Name, v.Prefix)
		}

		e := &pb.QueryResponse_Item{
			Identifier: v.Identifier,
			Text:       text,
			Subtext:    "",
			Actions:    []string{"search"},
			Icon:       v.Icon,
			Provider:   Name,
			Score:      int32(len(config.Engines) - k),
			Type:       0,
		}

		if query != "" {
			score, pos, start := common.FuzzyScore(query, v.Name, exact)

			e.Score = score
			e.Fuzzyinfo = &pb.QueryResponse_Item_FuzzyInfo{
				Field:     "text",
				Positions: pos,
				Start:     start,
			}
		}

		var usageScore int32
		if config.History {
			if e.Score > config.MinScore || query == "" && config.HistoryWhenEmpty {
				usageScore = h.CalcUsageScore(query, e.Identifier)

				if usageScore != 0 {
					e.State = append(e.State, "history")
					e.Actions = append(e.Actions, history.ActionDelete)
				}

				e.Score = e.Score + usageScore
			}
		}

		if e.Score > config.MinScore || query == "" {
			entries = append(entries, e)
		}
	}

	return entries
}

func likelyAddress(query string) bool {
	if !strings.Contains(query, ".") &&
		!strings.Contains(query, "://") {
		return false
	}

	if strings.Contains(query, " ") ||
		strings.HasSuffix(query, ".") ||
		strings.HasPrefix(query, ".") {
		return false
	}

	if !strings.Contains(query, "://") {
		query = fmt.Sprintf("https://%s", query)
	}

	_, err := url.Parse(query)

	return err == nil
}

func queryEngines(prefix string, query string, single bool, exact bool, conn net.Conn, format uint8) []*pb.QueryResponse_Item {
	entries := []*pb.QueryResponse_Item{}

	queriedEngines := []Engine{}
	if prefix == "" {
		for _, engine := range config.Engines {
			if (!single && engine.Default) || (single && engine.DefaultSingle) {
				queriedEngines = append(queriedEngines, engine)
			}
		}
	} else {
		for _, engine := range config.Engines {
			if engine.Prefix == prefix {
				queriedEngines = append(queriedEngines, engine)
			}
		}
	}

	// Direct search
	for i, engine := range queriedEngines {

		searchEntry := &pb.QueryResponse_Item{
			Identifier: engine.Identifier,
			Text:       engine.Name,
			Subtext:    "",
			Actions:    []string{"search"},
			Icon:       engine.Icon,
			Provider:   Name,
			Score:      int32(len(queriedEngines) - i),
			Type:       0,
		}

		if query != "" {
			searchEntry.Text = config.TextPrefix + query
			searchEntry.Subtext = engine.Name
		}

		if single && engine.DefaultSingle || prefix != "" {
			searchEntry.Score += 1_000
		}

		entries = append(entries, searchEntry)
	}

	// API suggestions
	if single || prefix != "" {
		slots := createPlaceholderSlots(queriedEngines, prefix, query)
		entries = append(entries, slots...)

		scheduleSuggestionsAsync(queriedEngines, prefix, query, conn, format)
	}

	// Browser history suggestions
	if (single || prefix != "") && config.BrowserProfilePath != "" && query != "" && prefix != config.EngineFinderPrefix {
		filterByHost := prefix != "" && prefix != config.EngineFinderPrefix

		historySlots := createBrowserHistoryPlaceholders(queriedEngines, prefix, query)
		entries = append(entries, historySlots...)

		scheduleBrowserHistoryAsync(query, queriedEngines, filterByHost, conn, format)
	}

	// Engines finder
	isPrefix := prefix == config.EngineFinderPrefix && prefix != ""
	isDefault := prefix == "" && !single && config.EngineFinderDefault
	isDefaultSingle := prefix == "" && single && config.EngineFinderDefaultSingle
	if isPrefix || isDefault || isDefaultSingle {
		entries = append(entries, listEngines(query, single, exact)...)
	}

	return entries
}

func queryEmpty(single bool, exact bool) []*pb.QueryResponse_Item {
	entries := []*pb.QueryResponse_Item{}

	entries = append(entries, listEngines("", single, exact)...)

	// TODO Add configurable empty behavior
	// TODO List Recent Browser History
	// TODO List History Browser by frecency
	// TODO List Bookmarks/Pins

	return entries
}

func Query(conn net.Conn, query string, single bool, exact bool, format uint8) []*pb.QueryResponse_Item {
	entries := []*pb.QueryResponse_Item{}
	prefix, query := splitEnginePrefix(query)

	// Address Query
	if likelyAddress(query) && prefix == "" {
		address := query
		if !strings.Contains(address, "://") {
			address = fmt.Sprintf("https://%s", query)
		}

		addressEntry := &pb.QueryResponse_Item{
			Identifier: "websearch",
			Text:       fmt.Sprintf("visit: %s", address),
			Actions:    []string{"open_url"},
			Icon:       Icon(),
			Provider:   Name,
			Score:      1_000_000,
		}
		entries = append(entries, addressEntry)
	}

	// Search Query
	if config.EnginesAsActions {
		actions := make([]string, len(config.Engines))
		for i, engine := range config.Engines {
			actions[i] = engine.Name
		}

		actionEntry := &pb.QueryResponse_Item{
			Identifier: "websearch",
			Text:       fmt.Sprintf("%s%s", config.TextPrefix, query),
			Actions:    actions,
			Icon:       Icon(),
			Provider:   Name,
			Score:      1,
			Type:       0,
		}
		entries = append(entries, actionEntry)
	} else {
		if query == "" && prefix == "" {
			entries = append(entries, queryEmpty(single, exact)...)
		} else {
			entries = append(entries, queryEngines(prefix, query, single, exact, conn, format)...)
		}
	}

	// Boost results when queried with prefix
	if prefix != "" {
		for _, entry := range entries {
			entry.Score += 10_000
		}
	}

	return entries
}
