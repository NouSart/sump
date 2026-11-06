package spv2

import (
	"sort"
	"sync"
)

// Metric 是一个指标样本。
type Metric struct {
	Name    string            `msgpack:"name" json:"name"`
	Type    string            `msgpack:"type" json:"type"` // counter|gauge|histogram
	Help    string            `msgpack:"help,omitempty" json:"help,omitempty"`
	Unit    string            `msgpack:"unit,omitempty" json:"unit,omitempty"`
	Labels  map[string]string `msgpack:"labels,omitempty" json:"labels,omitempty"`
	Value   float64           `msgpack:"value,omitempty" json:"value,omitempty"`
	Count   uint64            `msgpack:"count,omitempty" json:"count,omitempty"`
	Sum     float64           `msgpack:"sum,omitempty" json:"sum,omitempty"`
	Buckets []Bucket          `msgpack:"buckets,omitempty" json:"buckets,omitempty"`
}

// Bucket 是直方图桶。
type Bucket struct {
	LE    float64 `msgpack:"le" json:"le"`
	Count uint64  `msgpack:"count" json:"count"`
}

type series struct {
	metric  Metric
	buckets []float64
}

// Registry 是极简指标注册表。
type Registry struct {
	mu     sync.Mutex
	series map[string]*series
}

// NewRegistry 创建注册表。
func NewRegistry() *Registry {
	return &Registry{series: map[string]*series{}}
}

func key(name string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := name
	for _, k := range keys {
		out += "|" + k + "=" + labels[k]
	}
	return out
}

func copyLabels(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels))
	for k, v := range labels {
		out[k] = v
	}
	return out
}

func (r *Registry) get(name, typ, help, unit string, labels map[string]string) *series {
	k := key(name, labels)
	if s, ok := r.series[k]; ok {
		return s
	}
	s := &series{metric: Metric{Name: name, Type: typ, Help: help, Unit: unit, Labels: copyLabels(labels)}}
	r.series[k] = s
	return s
}

// Counter 累加计数。
func (r *Registry) Counter(name, help string, labels map[string]string, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(name, "counter", help, "1", labels)
	s.metric.Value += delta
}

// AddGauge 增减当前值。
func (r *Registry) AddGauge(name, help string, labels map[string]string, delta float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(name, "gauge", help, "", labels)
	s.metric.Value += delta
}

// Gauge 设置当前值。
func (r *Registry) Gauge(name, help string, labels map[string]string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(name, "gauge", help, "", labels)
	s.metric.Value = value
}

// Observe 记录一个直方图样本。
func (r *Registry) Observe(name, help, unit string, labels map[string]string, value float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.get(name, "histogram", help, unit, labels)
	if s.buckets == nil {
		s.buckets = []float64{0.005, 0.01, 0.05, 0.1, 0.5, 1, 5}
	}
	s.metric.Count++
	s.metric.Sum += value
	if s.metric.Buckets == nil {
		s.metric.Buckets = make([]Bucket, len(s.buckets))
		for i, le := range s.buckets {
			s.metric.Buckets[i] = Bucket{LE: le}
		}
	}
	for i := range s.buckets {
		if value <= s.buckets[i] {
			s.metric.Buckets[i].Count++
		}
	}
}

// Snapshot 返回指标快照。
func (r *Registry) Snapshot() []Metric {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Metric, 0, len(r.series))
	for _, s := range r.series {
		m := s.metric
		m.Labels = copyLabels(s.metric.Labels)
		if s.buckets != nil {
			m.Buckets = append([]Bucket(nil), s.metric.Buckets...)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return key("", out[i].Labels) < key("", out[j].Labels)
	})
	return out
}
