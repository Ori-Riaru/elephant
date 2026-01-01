package main

import (
	"crypto/md5"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"

	"github.com/abenz1267/elephant/v2/internal/comm/handlers"
	"github.com/abenz1267/elephant/v2/internal/util"
	"github.com/abenz1267/elephant/v2/pkg/common"
	"github.com/abenz1267/elephant/v2/pkg/common/history"
	"github.com/abenz1267/elephant/v2/pkg/pb/pb"
	_ "github.com/mattn/go-sqlite3"
)

var (
	Name                = "websearch"
	NamePretty          = "Web Search"
	config              *Config
	h                   = history.Load(Name)
	engineNameMap       = make(map[string]*Engine)
	engineIdentifierMap = make(map[string]*Engine)
)

//go:embed README.md
var readme string

type Config struct {
	common.Config      `koanf:",squash"`
	Command            string   `koanf:"command" desc:"default command to be executed. supports %VALUE%." default:"xdg-open"`
	Engines            []Engine `koanf:"entries" desc:"entries" default:"google"`
	BrowserProfilePath string   `koanf:"browser_history_path" desc:"path to browser profile" default:""`
	TextPrefix         string   `koanf:"text_prefix" desc:"text prefix for search entries" default:"Search: "`
	AlwaysShowDefault  bool     `koanf:"always_show_default" desc:"show default search engine when multiple providers are queried" default:"false"`
	EnginesAsActions   bool     `koanf:"engines_as_actions" desc:"run engines as actions" default:"true"`

	EngineFinderPrefix        string `koanf:"engine_finder_prefix" desc:"prefix for explicitly querying the engine finder" default:"@e"`
	EngineFinderDefault       bool   `koanf:"engine_finder_default" desc:"include engine finder results when searching with no engine prefix" default:"false"`
	EngineFinderDefaultSingle bool   `koanf:"engine_finder_default_single" desc:"display by default when no engine prefix" default:"true"`
	History                   bool   `koanf:"history" desc:"consider usage history for engine sorting" default:"true"`
	HistoryWhenEmpty          bool   `koanf:"history_when_empty" desc:"consider usage history when query is empty" default:"false"`

	SuggestionsDebounce int `koanf:"suggestions_debounce" desc:"debounce delay for async items (ms)" default:"100"`
	SuggestionsTimeout  int `koanf:"suggestions_timeout" desc:"timeout at which a suggestion query will be dropped" default:"1000"`
	MaxApiItems         int `koanf:"max_api_items" desc:"maximum final number of api suggestion items" default:"4"`
	MaxBrowserItems     int `koanf:"browser_history_limit" desc:"max browser history suggestions to return" default:"6"`
}

type Engine struct {
	Identifier      string
	Name            string `koanf:"name" desc:"name of the entry" default:""`
	Icon            string `koanf:"icon" desc:"icon to display, fallsback to global" default:""`
	Prefix          string `koanf:"prefix" desc:"prefix to actively trigger this entry" default:""`
	URL             string `koanf:"url" desc:"url, example: 'https://www.google.com/search?q=%TERM%'" default:""`
	SuggestionsURL  string `koanf:"suggestions_url" desc:"API endpoint for suggestions" default:""`
	SuggestionsPath string `koanf:"suggestions_path" desc:"JSON path to extract suggestions" default:"1"`
	Default         bool   `koanf:"default" desc:"display by default when querying multiple providers" default:"false"`
	DefaultSingle   bool   `koanf:"default_single" desc:"display by default when querying only the websearch provider" default:"false"`
}

type Suggestion struct {
	Identifier string
	Content    string
	Engine     Engine
	Score      int32
}

func Setup() {
	config = &Config{
		Config: common.Config{
			Icon:     "applications-internet",
			MinScore: 20,
		},
		Command:            "xdg-open",
		BrowserProfilePath: "/home/riaru/.mozilla/firefox/riaru", // FIXME: remove hardcoded
		TextPrefix:         "Search: ",
		AlwaysShowDefault:  true,
		EnginesAsActions:   false,

		EngineFinderPrefix:        "@e",
		EngineFinderDefault:       false,
		EngineFinderDefaultSingle: true,
		History:                   true,
		HistoryWhenEmpty:          false,

		SuggestionsDebounce: 80,
		SuggestionsTimeout:  1000,
		MaxBrowserItems:     6,
		MaxApiItems:         4, // TODO Adjust for default theme
	}

	common.LoadConfig(Name, config)

	if config.NamePretty != "" {
		NamePretty = config.NamePretty
	}

	handlers.WebsearchAlwaysShow = config.AlwaysShowDefault

	if len(config.Engines) == 0 {
		config.Engines = append(config.Engines,
			Engine{
				Name:    "Google",
				Default: true,
				URL:     "https://www.google.com/search?q=%TERM%",
			},
		)
	}

	if len(config.Engines) == 1 {
		config.Engines[0].Default = true
		config.Engines[0].DefaultSingle = true
	}

	for k, v := range config.Engines {
		config.Engines[k].Identifier = hashEngineIdentifier(v)
		engineNameMap[v.Name] = &config.Engines[k]
		engineIdentifierMap[config.Engines[k].Identifier] = &config.Engines[k]

		if v.Icon == "" {
			config.Engines[k].Icon = config.Config.Icon
		}

		if v.SuggestionsPath == "" {
			config.Engines[k].SuggestionsPath = "1" // Assume open search format by default
		}

		if v.Prefix != "" {
			handlers.WebsearchPrefixes[v.Prefix] = v.Name
		}

		if v.Default {
			handlers.MaxGlobalItemsToDisplayWebsearch++
		}
	}

	if config.BrowserProfilePath != "" {
		path := config.BrowserProfilePath
		if !strings.HasSuffix(path, "places.sqlite") {
			path += "/places.sqlite"
		}

		var err error
		browserHistoryDB, err = sql.Open("sqlite3", path+"?mode=ro&_timeout=5000")
		if err != nil { // TODO Simplify Failed logic
			slog.Warn(Name, "browser_history", "failed to open database", "path", path, "error", err)
			browserHistoryDB = nil
		} else {
			browserHistoryDB.SetMaxOpenConns(1)
			browserHistoryDB.SetMaxIdleConns(1)

			if err = browserHistoryDB.Ping(); err != nil {
				slog.Warn(Name, "browser_history", "failed to ping database", "error", err)
				browserHistoryDB.Close()
				browserHistoryDB = nil
			} else {
				slog.Info(Name, "browser_history", "database opened successfully")
			}
		}
	}
}

func hashEngineIdentifier(engine Engine) string {
	hash := md5.Sum([]byte(engine.Name + engine.URL + engine.Prefix))
	return hex.EncodeToString(hash[:])
}

func splitEnginePrefix(query string) (string, string) {
	for _, engine := range config.Engines {
		if after, found := strings.CutPrefix(query, engine.Prefix); engine.Prefix != "" && found {
			return engine.Prefix, strings.TrimSpace(after)
		}
	}

	if after, found := strings.CutPrefix(query, config.EngineFinderPrefix); config.EngineFinderPrefix != "" && found {
		return config.EngineFinderPrefix, strings.TrimSpace(after)
	}

	return "", query
}

func Available() bool {
	return true
}

func PrintDoc() {
	fmt.Println(readme)
	fmt.Println()
	util.PrintConfig(Config{}, Name)
}

func Icon() string {
	return config.Icon
}

func HideFromProviderlist() bool {
	return config.HideFromProviderlist
}

func State(provider string) *pb.ProviderStateResponse {
	return &pb.ProviderStateResponse{}
}
