package litellm

import "github.com/prometheus/client_golang/prometheus"

// cacheWriteUnsplit counts calls whose cache-write tokens could not be split
// by cache lifetime and were billed as input instead (the fallback in
// MapPayload). A rising count means cache writes are billed at the input
// price, below what the provider charges.
var cacheWriteUnsplit *prometheus.CounterVec

func init() {
	c := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "velox_litellm_cache_write_unsplit_total",
		Help: "LiteLLM calls whose cache-write tokens had no usable 5m/1h split and were billed as input tokens. reason: no_split, sum_mismatch or not_in_prompt.",
	}, []string{"provider", "reason"})
	if err := prometheus.DefaultRegisterer.Register(c); err != nil {
		if are, ok := err.(prometheus.AlreadyRegisteredError); ok {
			cacheWriteUnsplit = are.ExistingCollector.(*prometheus.CounterVec)
		} else {
			panic(err)
		}
	} else {
		cacheWriteUnsplit = c
	}
}

// metricProvider bounds the provider label: the value comes from the
// caller's payload, so an unknown one is folded into "other".
func metricProvider(p string) string {
	switch p {
	case "anthropic", "bedrock", "bedrock_converse", "vertex_ai", "vertex_ai_beta", "openai", "azure":
		return p
	default:
		return "other"
	}
}
