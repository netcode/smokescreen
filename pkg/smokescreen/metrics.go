package smokescreen

import (
	"fmt"
	"time"

	"github.com/DataDog/datadog-go/statsd"
)

var metrics = map[string][]string{
	"acl.allow":                        {},
	"acl.decide_error":                 {},
	"acl.deny":                         {},
	"acl.report":                       {},
	"acl.role_not_determined":          {},
	"acl.unknown_error":                {},
	"cn.atpt.connect.time":             {},
	"cn.atpt.fail.total":               {},
	"cn.atpt.success.total":            {},
	"cn.atpt.total":                    {},
	"resolver.allow.default":           {},
	"resolver.allow.user_configured":   {},
	"resolver.attempts_total":          {},
	"resolver.deny.not_global_unicast": {},
	"resolver.deny.private_range":      {},
	"resolver.deny.user_configured":    {},
	"resolver.errors_total":            {},
}

// MetricsClient is a thin wrapper around statsd.ClientInterface. It is used to allow
// adding arbitrary tags to Smokescreen metrics.
type MetricsClient struct {
	additionalTags map[string][]string
	StatsdClient   statsd.ClientInterface
}

// NewMetricsClient creates a new MetricsClient with the provided statsd address and
// namespace.
func NewMetricsClient(addr, namespace string) (*MetricsClient, error) {
	c, err := statsd.New(addr)
	if err != nil {
		return nil, err
	}
	c.Namespace = namespace

	return &MetricsClient{
		additionalTags: make(map[string][]string),
		StatsdClient:   c,
	}, nil
}

// NewNoOpMetricsClient returns a MetricsClient with a no-op statsd client. This can
// be used when there's no statsd service available to smokescreen.
func NewNoOpMetricsClient() *MetricsClient {
	return &MetricsClient{
		additionalTags: make(map[string][]string),
		StatsdClient:   &statsd.NoOpClient{},
	}
}

// AddMetricTags associates the provided tags slice with a given metric. The metric must be present
// in the metrics slice.
func (mc *MetricsClient) AddMetricTags(metric string, mTags []string) error {
	if tags, ok := metrics[metric]; ok {
		metrics[metric] = append(tags, mTags...)
		return nil
	}
	return fmt.Errorf("unknown metric: %s", metric)
}

// GetMetricTags returns the slice of metrics associated with a given metric.
func (mc *MetricsClient) GetMetricTags(metric string) []string {
	if tags, ok := metrics[metric]; ok {
		return tags
	}
	return nil
}

func (mc *MetricsClient) Incr(metric string, rate float64) error {
	mTags := mc.GetMetricTags(metric)
	return mc.StatsdClient.Incr(metric, mTags, rate)
}

func (mc *MetricsClient) IncrWithTags(metric string, tags []string, rate float64) error {
	mTags := mc.GetMetricTags(metric)
	tags = append(tags, mTags...)
	return mc.StatsdClient.Incr(metric, tags, rate)
}

func (mc *MetricsClient) Timing(metric string, d time.Duration, rate float64) error {
	mTags := mc.GetMetricTags(metric)
	return mc.StatsdClient.Timing(metric, d, mTags, rate)
}

func (mc *MetricsClient) TimingWithTags(metric string, d time.Duration, rate float64, tags []string) error {
	mTags := mc.GetMetricTags(metric)
	tags = append(tags, mTags...)
	return mc.StatsdClient.Timing(metric, d, mTags, rate)
}
