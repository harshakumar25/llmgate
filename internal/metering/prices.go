package metering

import (
	"strings"

	"github.com/harshakumar25/llmgate/internal/config"
	"github.com/harshakumar25/llmgate/internal/providers"
)

// Price is USD per million tokens, input and output priced separately as
// every major API does. Output tokens usually cost several times more.
type Price struct {
	In, Out float64
}

// defaultPrices covers models whose list price is stable and well known.
// Anything else costs 0 until priced in config (price_in / price_out on the
// provider), which keeps the estimate honest rather than invented.
var defaultPrices = map[string]Price{
	"gpt-4o-mini": {In: 0.15, Out: 0.60},
	"gpt-4o":      {In: 2.50, Out: 10.00},
}

// Prices estimates request cost from per-provider overrides and the default
// table. The zero value prices every call at 0.
type Prices struct {
	byProvider map[string]Price
}

// NewPrices reads price_in / price_out overrides from the provider config.
func NewPrices(cfgs []config.Provider) *Prices {
	p := &Prices{byProvider: map[string]Price{}}
	for _, c := range cfgs {
		if c.PriceIn > 0 || c.PriceOut > 0 {
			p.byProvider[c.Name] = Price{In: c.PriceIn, Out: c.PriceOut}
		}
	}
	return p
}

// Cost returns the estimated USD cost of one call. Local providers (Ollama)
// are free unless priced explicitly.
func (p *Prices) Cost(provider, model string, u providers.Usage) float64 {
	var price Price
	if p != nil {
		if pp, ok := p.byProvider[provider]; ok {
			price = pp
		}
	}
	if price == (Price{}) && provider != "ollama" {
		price = lookup(model)
	}
	return (float64(u.PromptTokens)*price.In + float64(u.CompletionTokens)*price.Out) / 1e6
}

// lookup matches exact names first, then dated snapshots such as
// "gpt-4o-mini-2024-07-18" by their longest known prefix.
func lookup(model string) Price {
	if p, ok := defaultPrices[model]; ok {
		return p
	}
	best := ""
	for name := range defaultPrices {
		if strings.HasPrefix(model, name+"-") && len(name) > len(best) {
			best = name
		}
	}
	return defaultPrices[best]
}
