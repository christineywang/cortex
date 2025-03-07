// Copyright 2016 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Mostly lifted from prometheus/web/api/v1/api.go.

package queryrange

import (
	"context"
	"flag"
	"net/http"
	"strings"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/weaveworks/common/user"

	"github.com/cortexproject/cortex/pkg/chunk/cache"
	"github.com/cortexproject/cortex/pkg/querier/frontend"
)

const day = 24 * time.Hour

// Config for query_range middleware chain.
type Config struct {
	SplitQueriesByInterval time.Duration `yaml:"split_queries_by_interval"`
	SplitQueriesByDay      bool          `yaml:"split_queries_by_day"`
	AlignQueriesWithStep   bool          `yaml:"align_queries_with_step"`
	ResultsCacheConfig     `yaml:"results_cache"`
	CacheResults           bool `yaml:"cache_results"`
	MaxRetries             int  `yaml:"max_retries"`
}

// RegisterFlags adds the flags required to config this to the given FlagSet.
func (cfg *Config) RegisterFlags(f *flag.FlagSet) {
	f.IntVar(&cfg.MaxRetries, "querier.max-retries-per-request", 5, "Maximum number of retries for a single request; beyond this, the downstream error is returned.")
	f.BoolVar(&cfg.SplitQueriesByDay, "querier.split-queries-by-day", false, "Deprecated: Split queries by day and execute in parallel.")
	f.DurationVar(&cfg.SplitQueriesByInterval, "querier.split-queries-by-interval", 0, "Split queries by an interval and execute in parallel, 0 disables it. You should use an a multiple of 24 hours (same as the storage bucketing scheme), to avoid queriers downloading and processing the same chunks.")
	f.BoolVar(&cfg.AlignQueriesWithStep, "querier.align-querier-with-step", false, "Mutate incoming queries to align their start and end with their step.")
	f.BoolVar(&cfg.CacheResults, "querier.cache-results", false, "Cache query results.")
	cfg.ResultsCacheConfig.RegisterFlags(f)
}

// HandlerFunc is like http.HandlerFunc, but for Handler.
type HandlerFunc func(context.Context, Request) (Response, error)

// Do implements Handler.
func (q HandlerFunc) Do(ctx context.Context, req Request) (Response, error) {
	return q(ctx, req)
}

// Handler is like http.Handle, but specifically for Prometheus query_range calls.
type Handler interface {
	Do(context.Context, Request) (Response, error)
}

// MiddlewareFunc is like http.HandlerFunc, but for Middleware.
type MiddlewareFunc func(Handler) Handler

// Wrap implements Middleware.
func (q MiddlewareFunc) Wrap(h Handler) Handler {
	return q(h)
}

// Middleware is a higher order Handler.
type Middleware interface {
	Wrap(Handler) Handler
}

// MergeMiddlewares produces a middleware that applies multiple middleware in turn;
// ie Merge(f,g,h).Wrap(handler) == f.Wrap(g.Wrap(h.Wrap(handler)))
func MergeMiddlewares(middleware ...Middleware) Middleware {
	return MiddlewareFunc(func(next Handler) Handler {
		for i := len(middleware) - 1; i >= 0; i-- {
			next = middleware[i].Wrap(next)
		}
		return next
	})
}

// NewTripperware returns a Tripperware configured with middlewares to limit, align, split, retry and cache requests.
func NewTripperware(cfg Config, log log.Logger, limits Limits, codec Codec, cacheExtractor Extractor) (frontend.Tripperware, cache.Cache, error) {
	queryRangeMiddleware := []Middleware{LimitsMiddleware(limits)}
	if cfg.AlignQueriesWithStep {
		queryRangeMiddleware = append(queryRangeMiddleware, InstrumentMiddleware("step_align"), StepAlignMiddleware)
	}
	// SplitQueriesByDay is deprecated use SplitQueriesByInterval.
	if cfg.SplitQueriesByDay {
		level.Warn(log).Log("msg", "flag querier.split-queries-by-day (or config split_queries_by_day) is deprecated, use querier.split-queries-by-interval instead.")
		cfg.SplitQueriesByInterval = day
	}
	if cfg.SplitQueriesByInterval != 0 {
		queryRangeMiddleware = append(queryRangeMiddleware, InstrumentMiddleware("split_by_interval"), SplitByIntervalMiddleware(cfg.SplitQueriesByInterval, limits, codec))
	}
	var c cache.Cache
	if cfg.CacheResults {
		queryCacheMiddleware, cache, err := NewResultsCacheMiddleware(log, cfg.ResultsCacheConfig, limits, codec, cacheExtractor)
		if err != nil {
			return nil, nil, err
		}
		c = cache
		queryRangeMiddleware = append(queryRangeMiddleware, InstrumentMiddleware("results_cache"), queryCacheMiddleware)
	}
	if cfg.MaxRetries > 0 {
		queryRangeMiddleware = append(queryRangeMiddleware, InstrumentMiddleware("retry"), NewRetryMiddleware(log, cfg.MaxRetries))
	}

	return frontend.Tripperware(func(next http.RoundTripper) http.RoundTripper {
		// Finally, if the user selected any query range middleware, stitch it in.
		if len(queryRangeMiddleware) > 0 {
			return NewRoundTripper(next, codec, queryRangeMiddleware...)
		}
		return next
	}), c, nil
}

type roundTripper struct {
	next    http.RoundTripper
	handler Handler
	codec   Codec
}

// NewRoundTripper merges a set of middlewares into an handler, then inject it into the `next` roundtripper
// using the codec to translate requests and responses.
func NewRoundTripper(next http.RoundTripper, codec Codec, middlewares ...Middleware) http.RoundTripper {
	transport := roundTripper{
		next:  next,
		codec: codec,
	}
	transport.handler = MergeMiddlewares(middlewares...).Wrap(&transport)
	return transport
}

func (q roundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.URL.Path, "/query_range") {
		return q.next.RoundTrip(r)
	}

	request, err := q.codec.DecodeRequest(r.Context(), r)
	if err != nil {
		return nil, err
	}

	LogToSpan(r.Context(), request)

	response, err := q.handler.Do(r.Context(), request)
	if err != nil {
		return nil, err
	}

	return q.codec.EncodeResponse(r.Context(), response)
}

// Do implements Handler.
func (q roundTripper) Do(ctx context.Context, r Request) (Response, error) {
	request, err := q.codec.EncodeRequest(ctx, r)
	if err != nil {
		return nil, err
	}

	if err := user.InjectOrgIDIntoHTTPRequest(ctx, request); err != nil {
		return nil, err
	}

	response, err := q.next.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()

	return q.codec.DecodeResponse(ctx, response, r)
}
