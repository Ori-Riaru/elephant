package common

import (
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/charlievieth/fastwalk"
	"github.com/pelletier/go-toml/v2"

	lua "github.com/yuin/gopher-lua"
)

type MenuConfig struct {
	Config `koanf:",squash"`
	Paths  []string `koanf:"paths" desc:"additional paths to check for menu definitions." default:""`
}

type Menu struct {
	HideFromProviderlist bool     `toml:"hide_from_providerlist" desc:"hides a provider from the providerlist provider. provider provider." default:"false"`
	Name                 string   `toml:"name" desc:"name of the menu"`
	NamePretty           string   `toml:"name_pretty" desc:"prettier name you usually want to display to the user."`
	Description          string   `toml:"description" desc:"used as a subtext"`
	Icon                 string   `toml:"icon" desc:"default icon"`
	Action               string   `toml:"action" desc:"default menu action to use"`
	SearchName           bool     `toml:"search_name" desc:"wether to search for the menu name as well when searching globally" default:"false"`
	Cache                bool     `toml:"cache" desc:"will cache the results of the lua script on startup"`
	Entries              []Entry  `toml:"entries" desc:"menu items"`
	Terminal             bool     `toml:"terminal" desc:"execute action in terminal or not"`
	Keywords             []string `toml:"keywords" desc:"searchable keywords"`
	FixedOrder           bool     `toml:"fixed_order" desc:"don't sort entries alphabetically"`
	History              bool     `toml:"history" desc:"make use of history for sorting"`
	HistoryWhenEmpty     bool     `toml:"history_when_empty" desc:"consider history when query is empty"`
	MinScore             int32    `toml:"min_score" desc:"minimum score for items to be displayed" default:"depends on provider"`
	Parent               string   `toml:"parent" desc:"defines the parent menu" default:""`

	// internal
	luaString string
	IsLua     bool `toml:"-"`
	luaState  *lua.LState
}

func (m *Menu) newLuaState() {
	l := lua.NewState()

	if err := l.DoString(m.luaString); err != nil {
		slog.Error(m.Name, "newLuaState", err)
		l.Close()
		return
	}

	if l == nil {
		slog.Error(m.Name, "newLuaState", "lua state is nil")
		return
	}

	m.luaState = l
}

func (m *Menu) CreateLuaEntries() {
	if m.luaState == nil {
		slog.Error(m.Name, "CreateLuaEntries", "no lua state")
		return
	}

	if err := m.luaState.CallByParam(lua.P{
		Fn:      m.luaState.GetGlobal("GetEntries"),
		NRet:    1,
		Protect: true,
	}); err != nil {
		slog.Error(m.Name, "GetLuaEntries", err)
		return
	}

	res := []Entry{}

	ret := m.luaState.Get(-1)
	m.luaState.Pop(1)

	if table, ok := ret.(*lua.LTable); ok {
		table.ForEach(func(key, value lua.LValue) {
			if item, ok := value.(*lua.LTable); ok {
				entry := Entry{}

				if text := item.RawGetString("Text"); text != lua.LNil {
					entry.Text = string(text.(lua.LString))
				}

				if preview := item.RawGetString("Preview"); preview != lua.LNil {
					entry.Preview = string(preview.(lua.LString))
				}

				if subtext := item.RawGetString("Subtext"); subtext != lua.LNil {
					entry.Subtext = string(subtext.(lua.LString))
				}

				if val := item.RawGetString("Value"); val != lua.LNil {
					entry.Value = string(val.(lua.LString))
				}

				if icon := item.RawGetString("Icon"); icon != lua.LNil {
					entry.Icon = string(icon.(lua.LString))
				}

				if actions := item.RawGet(lua.LString("Actions")); actions != lua.LNil {
					if actionsTable, ok := actions.(*lua.LTable); ok {
						entry.Actions = make(map[string]string)
						actionsTable.ForEach(func(key, value lua.LValue) {
							if keyStr, keyOk := key.(lua.LString); keyOk {
								if valueStr, valueOk := value.(lua.LString); valueOk {
									entry.Actions[string(keyStr)] = string(valueStr)
								}
							}
						})
					}
				}

				entry.Identifier = entry.CreateIdentifier()
				entry.Menu = m.Name

				res = append(res, entry)
			}
		})
	}

	m.Entries = res

	go m.newLuaState()
}

type Entry struct {
	Text     string            `toml:"text" desc:"text for entry"`
	Async    string            `toml:"async" desc:"if the text should be updated asynchronously based on the action"`
	Subtext  string            `toml:"subtext" desc:"sub text for entry"`
	Value    string            `toml:"value" desc:"value to be used for the action."`
	Actions  map[string]string `toml:"actions" desc:"actions items can use"`
	Terminal bool              `toml:"terminal" desc:"runs action in terminal if true"`
	Icon     string            `toml:"icon" desc:"icon for entry"`
	SubMenu  string            `toml:"submenu" desc:"submenu to open, if has prefix 'dmenu:' it'll launch that dmenu"`
	Preview  string            `toml:"preview" desc:"filepath for the preview"`
	Keywords []string          `toml:"keywords" desc:"searchable keywords"`

	Identifier string `toml:"-"`
	Menu       string `toml:"-"`
}

// type LuaEntry struct {
// 	Text    string
// 	Preview string
// 	Subtext string
// 	Value   string
// 	Icon    string
// 	Menu    string
// }

func (e Entry) CreateIdentifier() string {
	md5 := md5.Sum(fmt.Appendf([]byte(""), "%s%s%s", e.Menu, e.Text, e.Value))
	return hex.EncodeToString(md5[:])
}

var (
	MenuConfigLoaded MenuConfig
	menuname         = "menus"
	Menus            = make(map[string]*Menu)
)

func LoadMenus() {
	MenuConfigLoaded = MenuConfig{
		Config: Config{
			MinScore: 10,
		},
		Paths: []string{},
	}

	LoadConfig(menuname, MenuConfigLoaded)

	for _, v := range ConfigDirs() {
		path := filepath.Join(v, "menus")
		MenuConfigLoaded.Paths = append(MenuConfigLoaded.Paths, path)
	}

	conf := fastwalk.Config{
		Follow: true,
	}

	for _, root := range MenuConfigLoaded.Paths {
		if _, err := os.Stat(root); err != nil {
			continue
		}

		if err := fastwalk.Walk(&conf, root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}

			if d.IsDir() {
				return nil
			}

			switch filepath.Ext(path) {
			case ".toml":
				createTomlMenu(path)
			case ".lua":
				createLuaMenu(path)
			}

			return nil
		}); err != nil {
			slog.Error(menuname, "walk", err)
			os.Exit(1)
		}
	}
}

func createLuaMenu(path string) {
	m := Menu{}
	m.IsLua = true

	b, err := os.ReadFile(path)
	if err != nil {
		slog.Error(m.Name, "lua read", err)
		return
	}

	m.luaString = string(b)

	m.newLuaState()

	if val := m.luaState.GetGlobal("Name"); val != lua.LNil {
		m.Name = string(val.(lua.LString))
	}

	if val := m.luaState.GetGlobal("NamePretty"); val != lua.LNil {
		m.NamePretty = string(val.(lua.LString))
	}

	if val := m.luaState.GetGlobal("HideFromProviderlist"); val != lua.LNil {
		m.HideFromProviderlist = bool(val.(lua.LBool))
	}

	if val := m.luaState.GetGlobal("Description"); val != lua.LNil {
		m.Description = string(val.(lua.LString))
	}

	if val := m.luaState.GetGlobal("Icon"); val != lua.LNil {
		m.Icon = string(val.(lua.LString))
	}

	if val := m.luaState.GetGlobal("Action"); val != lua.LNil {
		m.Action = string(val.(lua.LString))
	}

	if val := m.luaState.GetGlobal("SearchName"); val != lua.LNil {
		m.SearchName = bool(val.(lua.LBool))
	}

	if val := m.luaState.GetGlobal("Cache"); val != lua.LNil {
		m.Cache = bool(val.(lua.LBool))
	}

	if val := m.luaState.GetGlobal("Terminal"); val != lua.LNil {
		m.Terminal = bool(val.(lua.LBool))
	}

	if val := m.luaState.GetGlobal("Keywords"); val != lua.LNil {
		if table, ok := val.(*lua.LTable); ok {
			m.Keywords = make([]string, 0)
			table.ForEach(func(key, value lua.LValue) {
				if str, ok := value.(lua.LString); ok {
					m.Keywords = append(m.Keywords, string(str))
				}
			})
		}
	}

	if val := m.luaState.GetGlobal("FixedOrder"); val != lua.LNil {
		m.FixedOrder = bool(val.(lua.LBool))
	}

	if val := m.luaState.GetGlobal("History"); val != lua.LNil {
		m.History = bool(val.(lua.LBool))
	}

	if val := m.luaState.GetGlobal("HistoryWhenEmpty"); val != lua.LNil {
		m.HistoryWhenEmpty = bool(val.(lua.LBool))
	}

	if val := m.luaState.GetGlobal("MinScore"); val != lua.LNil {
		m.MinScore = int32(val.(lua.LNumber))
	}

	if val := m.luaState.GetGlobal("Parent"); val != lua.LNil {
		m.Parent = string(val.(lua.LString))
	}

	if m.Cache {
		m.CreateLuaEntries()
	}

	if m.Name == "" || m.NamePretty == "" {
		slog.Error("menus", "path", path, "error", "missing Name or NamePretty")
		return
	}

	Menus[m.Name] = &m
}

func createTomlMenu(path string) {
	m := Menu{}

	b, err := os.ReadFile(path)
	if err != nil {
		slog.Error(menuname, "setup", err)
	}

	err = toml.Unmarshal(b, &m)
	if err != nil {
		slog.Error(menuname, "setup", err)
	}

	for k, v := range m.Entries {
		m.Entries[k].Menu = m.Name
		m.Entries[k].Identifier = m.Entries[k].CreateIdentifier()

		if v.SubMenu != "" {
			m.Entries[k].Identifier = fmt.Sprintf("menus:%s", v.SubMenu)
		}
	}

	Menus[m.Name] = &m
}
