package session

import (
	"fmt"
	"strings"
	"sync"

	"github.com/lyraproj/hiera/merge"

	"github.com/lyraproj/dgo/vf"

	"github.com/lyraproj/dgo/typ"

	"github.com/lyraproj/dgo/dgo"
	"github.com/lyraproj/dgo/util"
	"github.com/lyraproj/hiera/config"
	"github.com/lyraproj/hiera/hieraapi"
	"github.com/lyraproj/hierasdk/hiera"
)

const hieraConfigsPrefix = `HieraConfig:`
const hieraLockPrefix = `HieraLock:`

type invocationMode byte

const (
	topLevelMode = invocationMode(iota)
	lookupOptionsMode
	dataMode
)

type ivContext struct {
	hieraapi.Session
	nameStack []string
	scope     dgo.Keyed
	luOpts    dgo.Map
	strategy  hieraapi.MergeStrategy
	configs   map[string]hieraapi.ResolvedConfig
	explainer hieraapi.Explainer
	mode      invocationMode
	redacted  bool
}

type nestedScope struct {
	parentScope dgo.Keyed
	scope       dgo.Keyed
}

func newInvocation(s hieraapi.Session, scope dgo.Keyed, explainer hieraapi.Explainer) hieraapi.Invocation {
	return &ivContext{
		Session:   s,
		nameStack: []string{},
		scope:     scope,
		configs:   map[string]hieraapi.ResolvedConfig{},
		explainer: explainer,
		mode:      topLevelMode}
}

func (ns *nestedScope) Get(key interface{}) dgo.Value {
	if v := ns.scope.Get(key); v != nil {
		return v
	}
	return ns.parentScope.Get(key)
}

func (ic *ivContext) Config(configPath string, moduleName string) hieraapi.ResolvedConfig {
	sc := ic.SharedCache()
	if configPath == `` {
		configPath = ic.SessionOptions().Get(hieraapi.HieraConfig).String()
	}

	if rc, ok := ic.configs[configPath]; ok {
		return rc
	}

	cp := hieraConfigsPrefix + configPath
	if val, ok := sc.Load(cp); ok {
		rc := Resolve(ic, val.(hieraapi.Config), moduleName)
		ic.configs[configPath] = rc
		return rc
	}

	lc := hieraLockPrefix + configPath

	myLock := sync.RWMutex{}
	myLock.Lock()

	var conf hieraapi.Config
	if lv, loaded := sc.LoadOrStore(lc, &myLock); loaded {
		// myLock was not stored so unlock it
		myLock.Unlock()

		if lock, ok := lv.(*sync.RWMutex); ok {
			// The loaded value is a lock. Wait for new config to be stored in place of
			// this lock
			lock.RLock()
			val, _ := sc.Load(cp)
			conf = val.(hieraapi.Config)
			lock.RUnlock()
		} else {
			conf = lv.(hieraapi.Config)
		}
	} else {
		conf = config.New(configPath)
		sc.Store(cp, conf)
		myLock.Unlock()
	}
	rc := Resolve(ic, conf, moduleName)
	ic.configs[configPath] = rc
	return rc
}

func (ic *ivContext) ExplainMode() bool {
	return ic.explainer != nil
}

func (ic *ivContext) LookupOptionsMode() bool {
	return ic.mode == lookupOptionsMode
}

func (ic *ivContext) DataMode() bool {
	return ic.mode == dataMode
}

func (ic *ivContext) extractConversion() (convertToType dgo.Type, convertToArgs dgo.Array) {
	lo := ic.luOpts
	if lo == nil {
		return
	}
	ct := lo.Get(`convert_to`)
	if ct == nil {
		return
	}
	var ts dgo.Value
	if cm, ok := ct.(dgo.Array); ok {
		// First arg must be a type. The rest is arguments
		switch cm.Len() {
		case 0:
			// Obviously bogus
		case 1:
			ts = cm.Get(0)
		default:
			ts = cm.Get(0)
			convertToArgs = cm.Slice(1, cm.Len())
		}
	} else {
		ts = ct
	}
	if ts != nil {
		convertToType = ic.Dialect().ParseType(ic.AliasMap(), ts.(dgo.String))
	}
	return
}

func (ic *ivContext) SetMergeStrategy(cliMergeOption dgo.Value, lookupOptions dgo.Map) {
	var opts dgo.Value
	if cliMergeOption != nil {
		ic.ReportMergeSource(`CLI option`)
		opts = cliMergeOption
	} else if lookupOptions != nil {
		if opts = lookupOptions.Get(`merge`); opts != nil {
			ic.ReportMergeSource(`"lookup_options" hash`)
		}
	}

	var mergeName string
	var mergeOpts dgo.Map
	switch opts := opts.(type) {
	case dgo.String:
		mergeName = opts.String()
	case dgo.Map:
		if mn, ok := opts.Get(`strategy`).(dgo.String); ok {
			mergeName = mn.String()
			mergeOpts = opts.Without(`strategy`)
		}
	default:
		mergeName = `first`
	}
	ic.luOpts = lookupOptions
	ic.strategy = merge.GetStrategy(mergeName, mergeOpts)
}

func (ic *ivContext) LookupAndConvertData(fn func() dgo.Value) dgo.Value {
	convertToType, convertToArgs := ic.extractConversion()

	var v dgo.Value
	if typ.Sensitive.Equals(convertToType) {
		ic.DoRedacted(func() { v = fn() })
	} else {
		v = fn()
	}

	if v != nil && convertToType != nil {
		if convertToArgs != nil {
			v = vf.Arguments(vf.Values(v).WithAll(convertToArgs))
		}
		v = vf.New(convertToType, v)
	}
	return v
}

func (ic *ivContext) MergeHierarchy(key hieraapi.Key, pvs []hieraapi.DataProvider, merge hieraapi.MergeStrategy) dgo.Value {
	return merge.MergeLookup(pvs, ic, func(pv interface{}) dgo.Value {
		pr := pv.(hieraapi.DataProvider)
		return ic.MergeLocations(key, pr, merge)
	})
}

func (ic *ivContext) MergeLocations(key hieraapi.Key, dh hieraapi.DataProvider, merge hieraapi.MergeStrategy) dgo.Value {
	return ic.WithDataProvider(dh, func() dgo.Value {
		locations := dh.Hierarchy().Locations()
		switch len(locations) {
		case 0:
			return ic.invokeWithLocation(dh, nil, key)
		case 1:
			return ic.invokeWithLocation(dh, locations[0], key)
		default:
			return merge.MergeLookup(locations, ic, func(location interface{}) dgo.Value {
				return ic.invokeWithLocation(dh, location.(hieraapi.Location), key)
			})
		}
	})
}

func (ic *ivContext) invokeWithLocation(dh hieraapi.DataProvider, location hieraapi.Location, key hieraapi.Key) dgo.Value {
	if location == nil {
		return dh.LookupKey(key, ic, nil)
	}
	return ic.WithLocation(location, func() dgo.Value {
		if location.Exists() {
			return dh.LookupKey(key, ic, location)
		}
		ic.ReportLocationNotFound()
		return nil
	})
}

func (ic *ivContext) Lookup(key hieraapi.Key, options dgo.Map) dgo.Value {
	rootKey := key.Root()
	if rootKey == `lookup_options` {
		return ic.WithInvalidKey(key, func() dgo.Value {
			ic.ReportNotFound(key)
			return nil
		})
	}

	v := ic.TopProvider()(ic.ServerContext(options), rootKey)
	if v != nil {
		dc := ic.ForData()
		v = dc.Interpolate(v, true)
		v = key.Dig(dc, v)
	}
	return v
}

func (ic *ivContext) WithKey(key hieraapi.Key, actor dgo.Producer) dgo.Value {
	if util.ContainsString(ic.nameStack, key.Source()) {
		panic(fmt.Errorf(`recursive lookup detected in [%s]`, strings.Join(ic.nameStack, `, `)))
	}
	ic.nameStack = append(ic.nameStack, key.Source())
	defer func() {
		ic.nameStack = ic.nameStack[:len(ic.nameStack)-1]
	}()
	return actor()
}

func (ic *ivContext) DoRedacted(doer dgo.Doer) {
	if ic.redacted {
		doer()
	} else {
		defer func() {
			ic.redacted = false
		}()
		ic.redacted = true
		doer()
	}
}

func (ic *ivContext) DoWithScope(scope dgo.Keyed, doer dgo.Doer) {
	sc := ic.scope
	ic.scope = scope
	doer()
	ic.scope = sc
}

func (ic *ivContext) Scope() dgo.Keyed {
	return ic.scope
}

// ServerContext creates and returns a new server context
func (ic *ivContext) ServerContext(options dgo.Map) hieraapi.ServerContext {
	return &serverCtx{ProviderContext: hiera.ProviderContextFromMap(options), invocation: ic}
}

func (ic *ivContext) WithDataProvider(p hieraapi.DataProvider, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushDataProvider(p)
	return actor()
}

func (ic *ivContext) WithInterpolation(expr string, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushInterpolation(expr)
	return actor()
}

func (ic *ivContext) WithInvalidKey(key interface{}, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushInvalidKey(key)
	return actor()
}

func (ic *ivContext) WithLocation(loc hieraapi.Location, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushLocation(loc)
	return actor()
}

func (ic *ivContext) WithLookup(key hieraapi.Key, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushLookup(key)
	return actor()
}

func (ic *ivContext) WithMerge(ms hieraapi.MergeStrategy, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushMerge(ms)
	return actor()
}

func (ic *ivContext) WithSegment(seg interface{}, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushSegment(seg)
	return actor()
}

func (ic *ivContext) WithSubLookup(key hieraapi.Key, actor dgo.Producer) dgo.Value {
	if ic.explainer == nil {
		return actor()
	}
	defer ic.explainer.Pop()
	ic.explainer.PushSubLookup(key)
	return actor()
}

func (ic *ivContext) ForConfig() hieraapi.Invocation {
	if ic.explainer == nil {
		return ic
	}
	lic := *ic
	lic.explainer = nil
	return &lic
}

func (ic *ivContext) ForData() hieraapi.Invocation {
	if ic.DataMode() {
		return ic
	}
	lic := *ic
	if !(lic.explainer == nil || !lic.explainer.OnlyOptions()) {
		lic.explainer = nil
	}
	lic.mode = dataMode
	return &lic
}

func (ic *ivContext) LookupOptions() dgo.Map {
	return ic.luOpts
}

func (ic *ivContext) MergeStrategy() hieraapi.MergeStrategy {
	return ic.strategy
}

func (ic *ivContext) ForLookupOptions() hieraapi.Invocation {
	if ic.LookupOptionsMode() {
		return ic
	}
	lic := *ic
	if !(ic.explainer == nil || ic.explainer.Options() || ic.explainer.OnlyOptions()) {
		lic.explainer = nil
	}
	lic.mode = lookupOptionsMode
	return &lic
}

func (ic *ivContext) ReportLocationNotFound() {
	if ic.explainer != nil {
		ic.explainer.AcceptLocationNotFound()
	}
}

func (ic *ivContext) ReportFound(key interface{}, value dgo.Value) {
	if ic.explainer != nil {
		ic.explainer.AcceptFound(key, value)
	}
}

func (ic *ivContext) ReportMergeResult(value dgo.Value) {
	if ic.explainer != nil {
		ic.explainer.AcceptMergeResult(value)
	}
}

func (ic *ivContext) ReportMergeSource(source string) {
	if ic.explainer != nil {
		ic.explainer.AcceptMergeSource(source)
	}
}

func (ic *ivContext) ReportNotFound(key interface{}) {
	if ic.explainer != nil {
		ic.explainer.AcceptNotFound(key)
	}
}

func (ic *ivContext) ReportText(messageProducer func() string) {
	if ic.explainer != nil {
		ic.explainer.AcceptText(messageProducer())
	}
}
