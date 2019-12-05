package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/lyraproj/dgo/dgo"
	"github.com/lyraproj/dgo/loader"
	"github.com/lyraproj/dgo/streamer"
	"github.com/lyraproj/dgo/tf"
	"github.com/lyraproj/dgo/vf"
	"github.com/lyraproj/hiera/hieraapi"
	"github.com/lyraproj/hiera/provider"
	"github.com/lyraproj/hierasdk/hiera"
)

type session struct {
	context.Context
	aliasMap dgo.AliasMap
	dialect  streamer.Dialect
	vars     map[string]interface{}
	scope    dgo.Keyed
	loader   dgo.Loader
}

const hieraCacheKey = `Hiera::Cache`
const hieraTopProviderKey = `Hiera::TopProvider`
const hieraSessionOptionsKey = `Hiera::SessionOptions`
const hieraTopProviderCacheKey = `Hiera::TopProvider::Cache`
const hieraPluginRegistry = `Hiera::Plugins`

// New creates a new Hiera Session which, among other things, holds on to a synchronized
// cache where all loaded things end up.
//
// parent: typically obtained using context.Background() but can be any context.
//
// topProvider: the topmost provider that defines the hierarchy
//
// options: a map[string]any of configuration options
func New(parent context.Context, topProvider hiera.LookupKey, oif interface{}, ldr dgo.Loader) hieraapi.Session {
	if topProvider == nil {
		topProvider = provider.ConfigLookupKey
	}

	options := vf.MutableMap()
	if oif != nil {
		options.PutAll(hieraapi.ToMap(`session options`, oif))
	}

	if options.Get(hieraapi.HieraConfig) == nil {
		var hieraRoot string
		if r := options.Get(hieraapi.HieraRoot); r != nil {
			hieraRoot = r.String()
		} else {
			var err error
			if hieraRoot, err = os.Getwd(); err != nil {
				panic(err)
			}
		}

		var fileName string
		if r := options.Get(hieraapi.HieraConfigFileName); r != nil {
			fileName = r.String()
		} else if config, ok := os.LookupEnv("HIERA_CONFIGFILE"); ok {
			fileName = config
		} else {
			fileName = `hiera.yaml`
		}
		options.Put(hieraapi.HieraConfig, filepath.Join(hieraRoot, fileName))
	}

	var dialect streamer.Dialect
	if ds, ok := options.Get(hieraapi.HieraDialect).(dgo.String); ok {
		switch ds.String() {
		case "dgo":
			dialect = streamer.DgoDialect()
		case "pcore":
			panic(errors.New(`pcore dialect is not yet implemented`))
		default:
			panic(fmt.Errorf(`unknown dialect '%s'`, ds))
		}
	}
	if dialect == nil {
		dialect = streamer.DgoDialect()
	}

	var scope dgo.Keyed
	if sv, ok := options.Get(hieraapi.HieraScope).(dgo.Keyed); ok {
		// Freeze scope if possible
		if f, ok := sv.(dgo.Freezable); ok {
			sv = f.FrozenCopy().(dgo.Keyed)
		}
		scope = sv
	} else {
		scope = vf.Map()
	}
	options.Freeze()

	vars := map[string]interface{}{
		hieraCacheKey:            &sync.Map{},
		hieraTopProviderKey:      topProvider,
		hieraTopProviderCacheKey: &sync.Map{},
		hieraSessionOptionsKey:   options,
		hieraPluginRegistry:      &pluginRegistry{}}

	s := &session{Context: parent, aliasMap: tf.NewAliasMap(), vars: vars, dialect: dialect, scope: scope}
	s.loader = s.newHieraLoader(ldr)
	return s
}

func (s *session) AliasMap() dgo.AliasMap {
	return s.aliasMap
}

func (s *session) Dialect() streamer.Dialect {
	return s.dialect
}

func (s *session) Invocation(si interface{}, explainer hieraapi.Explainer) hieraapi.Invocation {
	var scope dgo.Keyed
	if si == nil {
		scope = s.Scope()
	} else {
		scope = &nestedScope{s.Scope(), hieraapi.ToMap(`invocation scope`, si)}
	}
	return &ivContext{
		Session:    s,
		nameStack:  []string{},
		scope:      scope,
		configPath: s.SessionOptions().Get(hieraapi.HieraConfig).String(),
		explainer:  explainer}
}

// KillPlugins will ensure that all plugins started by this executable are gracefully terminated if possible or
// otherwise forcefully killed.
func (s *session) KillPlugins() {
	if pr := s.Get(hieraPluginRegistry); pr != nil {
		pr.(*pluginRegistry).stopAll()
	}
}

func (s *session) Loader() dgo.Loader {
	return s.loader
}

func (s *session) LoadFunction(he hieraapi.Entry) (fn dgo.Function, ok bool) {
	n := he.Function().Name()
	l := s.Loader()
	fn, ok = l.Namespace(`function`).Get(n).(dgo.Function)
	if ok {
		return
	}

	file := he.PluginFile()
	if file == `` {
		file = n
		if runtime.GOOS == `windows` {
			file += `.exe`
		}
	}

	var path string
	if filepath.IsAbs(file) {
		path = filepath.Clean(file)
	} else {
		path = filepath.Clean(filepath.Join(he.PluginDir(), file))
		abs, err := filepath.Abs(path)
		if err != nil {
			panic(err)
		}
		path = abs
	}

	l = l.Namespace(`plugin`)
	for _, pn := range strings.Split(path, string(os.PathSeparator)) {
		l = l.Namespace(pn)
		if l == nil {
			return nil, false
		}
	}

	fn, ok = l.Get(n).(dgo.Function)
	return
}

func (s *session) Scope() dgo.Keyed {
	return s.scope
}

func (s *session) Get(key string) interface{} {
	return s.vars[key]
}

func (s *session) TopProvider() hiera.LookupKey {
	if v, ok := s.Get(hieraTopProviderKey).(hiera.LookupKey); ok {
		return v
	}
	panic(notInitialized())
}

func (s *session) TopProviderCache() *sync.Map {
	if v, ok := s.Get(hieraTopProviderCacheKey).(*sync.Map); ok {
		return v
	}
	panic(notInitialized())
}

func (s *session) SessionOptions() dgo.Map {
	if v := s.Get(hieraSessionOptionsKey); v != nil {
		if g, ok := v.(dgo.Map); ok {
			return g
		}
	}
	panic(notInitialized())
}

func notInitialized() error {
	return errors.New(`session is not initialized`)
}

func (s *session) SharedCache() *sync.Map {
	if v, ok := s.Get(hieraCacheKey).(*sync.Map); ok {
		return v
	}
	panic(notInitialized())
}

func (s *session) newHieraLoader(p dgo.Loader) dgo.Loader {
	nsCreator := func(l dgo.Loader, name string) dgo.Loader {
		switch name {
		case `plugin`:
			return s.createPluginLoader(l)
		case `function`:
			return s.createFunctionLoader(l)
		default:
			return nil
		}
	}
	var l dgo.Loader
	if p == nil {
		l = loader.New(nil, ``, nil, nil, nsCreator)
	} else {
		l = p.NewChild(nil, nsCreator)
	}
	return l
}

func (s *session) createFunctionLoader(l dgo.Loader) dgo.Loader {
	m, ok := s.SessionOptions().Get(hieraapi.HieraFunctions).(dgo.Map)
	if !ok {
		m = vf.Map()
	}
	return loader.New(l, `function`, m, nil, nil)
}

func (s *session) createPluginLoader(p dgo.Loader) dgo.Loader {

	var pluginFinder = func(l dgo.Loader, _ string) interface{} {
		an := l.AbsoluteName()

		// Strip everything up to '/plugin/'
		ix := strings.Index(an, `/plugin/`)
		if ix < 0 {
			return nil
		}

		// Get the plugin registry for this session
		var allPlugins *pluginRegistry
		if pr := s.Get(hieraPluginRegistry); pr != nil {
			allPlugins = pr.(*pluginRegistry)
		} else {
			return nil
		}
		return allPlugins.startPlugin(an[ix+7:])
	}

	var pluginNamespace func(l dgo.Loader, name string) dgo.Loader
	pluginNamespace = func(l dgo.Loader, name string) dgo.Loader {
		return loader.New(l, name, nil, pluginFinder, pluginNamespace)
	}

	return loader.New(p, `plugin`, nil, pluginFinder, pluginNamespace)
}
