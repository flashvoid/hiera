package hieraapi

import (
	"github.com/lyraproj/pcore/px"
)

// ServerContext is the Hiera context used by lookup functions that operate in-process
type ServerContext interface {
	px.Value

	// Return the Option keyed by the given key or nil if no such option exists
	Option(key string) px.Value

	// Call the given func once for each key, value pair found in the options map.
	EachOption(func(key string, value px.Value))

	// ReportText will add the message returned by the given function to the
	// lookup explainer. The method will only get called when the explanation
	// support is enabled
	Explain(messageProducer func() string)

	// Cache adds the given key - value association to the cache
	Cache(key string, value px.Value) px.Value

	// CacheAll adds all key - value associations in the given hash to the cache
	CacheAll(hash px.OrderedMap)

	// CachedEntry returns the value for the given key together with
	// a boolean to indicate if the value was found or not
	CachedValue(key string) (px.Value, bool)

	// CachedEntries calls the consumer with each entry in the cache
	CachedEntries(consumer px.BiConsumer)

	// Interpolate resolves interpolations in the given value and returns the result
	Interpolate(value px.Value) px.Value

	// Invocation returns the active invocation.
	Invocation() Invocation
}
