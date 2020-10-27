package ratelimit

import (
	"fmt"
	"math/rand"
	"net/http"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/zalando/skipper/eskip"
	"github.com/zalando/skipper/filters"
	"github.com/zalando/skipper/routing"
)

const leakyBucketName = "leakyBucket"

// The leaky bucket is an algorithm based on an analogy of how a bucket with a constant leak will overflow if either
// the average rate at which water is poured in exceeds the rate at which the bucket leaks or if more water than
// the capacity of the bucket is poured in all at once.
//
// See https://en.wikipedia.org/wiki/Leaky_bucket
type leakyBucketShed struct {
	// Bucket manipulations in the shed must happen behind a locked door
	door sync.Mutex

	// Labeled buckets in form of timestamps of when bucket is expected to drain out.
	// Buckets are created and implicitly belong to a filter that defines bucket capacity and leak rate.
	buckets map[string]int64
}

type leakyBucketFilter struct {
	label    *temp
	capacity int64
	leaks    int64
	per      int64
	shed     *leakyBucketShed
}

type leakyBucketPreProcessor struct{}

func NewLeakyBucket() filters.Spec {
	shed := &leakyBucketShed{buckets: make(map[string]int64)}
	shed.scheduleRemoveEmpty()
	return shed
}

func (s *leakyBucketShed) Name() string {
	return leakyBucketName
}

func (s *leakyBucketShed) CreateFilter(args []interface{}) (filters.Filter, error) {
	if len(args) != 3 {
		return nil, filters.ErrInvalidFilterParameters
	}

	label, ok := args[0].(string)
	if !ok {
		return nil, filters.ErrInvalidFilterParameters
	}

	rate, ok := args[1].(string)
	if !ok {
		return nil, filters.ErrInvalidFilterParameters
	}
	n, d, err := parseRate(rate)
	if err != nil {
		return nil, err
	}

	var cap int64
	switch v := args[2].(type) {
	case int:
		cap = int64(v)
	case float64:
		cap = int64(v)
	default:
		return nil, filters.ErrInvalidFilterParameters
	}
	if cap < 0 {
		return nil, filters.ErrInvalidFilterParameters
	}
	return &leakyBucketFilter{label: newTemplate(label), capacity: cap, leaks: n, per: int64(d), shed: s}, nil
}

func (f *leakyBucketFilter) Request(ctx filters.FilterContext) {
	label := applyContext(f.label, ctx)
	if f.allow(label) {
		return
	}
	ctx.Serve(&http.Response{
		StatusCode: http.StatusTooManyRequests,
		// TODO: Header: "X-Rate-Limit", "Retry-After"
	})
}

func (*leakyBucketFilter) Response(filters.FilterContext) {}

func NewLeakyBucketPreProcessor() routing.PreProcessor {
	return &leakyBucketPreProcessor{}
}

// Checks filter configurations with the same label on different routes
func (p *leakyBucketPreProcessor) Do(routes []*eskip.Route) []*eskip.Route {
	defs := make(map[interface{}][]interface{})
	for _, route := range routes {
		for _, filter := range route.Filters {
			if filter.Name == leakyBucketName {
				label := filter.Args[0]
				if args, ok := defs[label]; ok {
					if !reflect.DeepEqual(args, filter.Args) {
						log.Errorf(`%s filters with same label "%s" have different arguments: %v vs %v`,
							leakyBucketName, label, args[1:], filter.Args[1:])
					}
				} else {
					defs[label] = filter.Args
				}
			}
		}
	}
	return routes
}

func (s *leakyBucketShed) scheduleRemoveEmpty() {
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				s.removeEmpty()
			}
		}
	}()
}

// Removes empty buckets
func (s *leakyBucketShed) removeEmpty() {
	// TODO: https://github.com/golang/go/issues/20135 "runtime: maps do not shrink after elements removal (delete)"
	// TODO: workaround buckets lock
	now := time.Now()
	nowNanos := now.UnixNano()

	s.door.Lock()
	before := len(s.buckets)

	for label, leaksUntil := range s.buckets {
		if leaksUntil < nowNanos {
			delete(s.buckets, label)
		}
	}

	after := len(s.buckets)
	s.door.Unlock()

	log.Infof("shed cleanup complete, before: %d, after: %d, removed: %d, duration: %v", before, after, before-after, time.Now().Sub(now))
}

// Returns true if labeled bucket has spare capacity
func (f *leakyBucketFilter) allow(label string) bool {
	f.shed.door.Lock()
	defer f.shed.door.Unlock()

	bucket := f.shed.buckets[label]
	if topped := f.addTo(bucket); topped > 0 {
		f.shed.buckets[label] = topped
		return true
	}
	return false
}

// Checks if bucket has spare capacity and returns new timestamp when bucket would drain out
// Returns 0 if bucket is full
func (f *leakyBucketFilter) addTo(leaksUntil int64) int64 {
	now := time.Now().UnixNano()
	if leaksUntil < now {
		// underflow: drained bucket (zero level)
		leaksUntil = now
	}
	// if level < capacity then top up
	// level = time*velocity = (leaksUntil-now)*leaks/per
	// To eliminate integer division level and capacity are multiplied by per>0 before comparison
	if (leaksUntil-now)*f.leaks < f.capacity*f.per {
		// top up 1 unit: it will drain in per/leaks time
		return leaksUntil + f.per/f.leaks
	}
	return 0
}

// Parses rate string in "number/duration" format (e.g. "10/m", "1/5s")
func parseRate(rate string) (n int64, per time.Duration, err error) {
	s := strings.SplitN(rate, "/", 2)
	if len(s) != 2 {
		err = fmt.Errorf(`rate %s doesn't match the "number/duration" format (e.g. 10/m, 1/5s)`, rate)
		return
	}

	n, err = strconv.ParseInt(s[0], 10, 64)
	if err != nil {
		return
	}
	if n <= 0 {
		err = fmt.Errorf(`number %d in rate "number/duration" format must be positive`, n)
		return
	}

	switch s[1] {
	case "ns", "us", "µs", "ms", "s", "m", "h":
		s[1] = "1" + s[1]
	}
	per, err = time.ParseDuration(s[1])
	if err != nil {
		return
	}
	if per <= 0 {
		err = fmt.Errorf(`duration %v in rate "number/duration" format must be positive`, per)
	}
	return
}

func applyContext(t *temp, ctx filters.FilterContext) string {
	return t.apply(func(key string) string {
		// for testing
		if r := strings.TrimPrefix(key, "rand."); r != key {
			n, err := strconv.ParseInt(r, 10, 64)
			if err == nil {
				return strconv.FormatInt(rand.Int63n(n), 10)
			} else {
				return ""
			}
		}

		if h := strings.TrimPrefix(key, "request.header."); h != key {
			return ctx.Request().Header.Get(h)
		}
		if q := strings.TrimPrefix(key, "request.url_query."); q != key {
			return ctx.Request().URL.Query().Get(q)
		}
		if c := strings.TrimPrefix(key, "request.cookie."); c != key {
			if cookie, err := ctx.Request().Cookie(c); err == nil {
				return cookie.Value
			}
			return ""
		}
		if v := ctx.PathParam(key); v != "" {
			return v
		}
		return ""
	})
}

// TODO: use eskip.Template, needs pattern extension
var placeholderRegexp = regexp.MustCompile(`\$\{([^{}]+)\}`)

type temp struct {
	template     string
	placeholders []string
}

func newTemplate(template string) *temp {
	matches := placeholderRegexp.FindAllStringSubmatch(template, -1)
	placeholders := make([]string, len(matches))

	for index, placeholder := range matches {
		placeholders[index] = placeholder[1]
	}
	return &temp{template: template, placeholders: placeholders}
}

func (t *temp) apply(get func(string) string) string {
	result := t.template
	for _, placeholder := range t.placeholders {
		result = strings.Replace(result, "${"+placeholder+"}", get(placeholder), -1)
	}
	return result
}
