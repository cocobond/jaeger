// Copyright (c) 2018 The Jaeger Authors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-logr/zapr"
	"github.com/jaegertracing/jaeger/internal/jaegerclientenv2otel"
	"github.com/jaegertracing/jaeger/internal/telemetry/otelsemconv"
	"github.com/jaegertracing/jaeger/internal/tracegen"
	"github.com/jaegertracing/jaeger/internal/version"
	"go.opentelemetry.io/contrib/samplers/jaegerremote"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

var flagAdaptiveSamplingEndpoint string

func main() {
	zc := zap.NewDevelopmentConfig()
	zc.Level = zap.NewAtomicLevelAt(zapcore.Level(-8)) // level used by OTEL's Debug()
	logger, err := zc.Build()
	if err != nil {
		panic(err)
	}
	otel.SetLogger(zapr.NewLogger(logger))

	fs := flag.CommandLine
	cfg := new(tracegen.Config)
	cfg.Flags(fs)
	fs.StringVar(
		&flagAdaptiveSamplingEndpoint,
		"adaptive-sampling",
		"",
		"HTTP endpoint to use to retrieve sampling strategies, "+
			"e.g. http://localhost:14268/api/sampling. "+
			"When not specified a standard SDK sampler will be used "+
			"(see OTEL_TRACES_SAMPLER env var in OTEL docs)")
	flag.Parse()

	logger.Info(version.Get().String())

	otel.SetTextMapPropagator(propagation.TraceContext{})
	jaegerclientenv2otel.MapJaegerToOtelEnvVars(logger)

	tracers, shutdown := createTracers(cfg, logger)
	defer shutdown(context.Background())

	tracegen.Run(cfg, tracers, logger)
}

func createTracers(cfg *tracegen.Config, logger *zap.Logger) ([]trace.Tracer, func(context.Context) error) {
	if cfg.Services < 1 {
		cfg.Services = 1
	}
	var shutdown []func(context.Context) error
	var tracers []trace.Tracer
	for s := 0; s < cfg.Services; s++ {
		svc := cfg.Service
		if cfg.Services > 1 {
			svc = fmt.Sprintf("%s-%02d", svc, s)
		}

		exp, err := createOtelExporter(cfg.TraceExporter)
		if err != nil {
			logger.Sugar().Fatalf("cannot create trace exporter %s: %s", cfg.TraceExporter, err)
		}
		logger.Sugar().Infof("using %s trace exporter for service %s", cfg.TraceExporter, svc)

		res, err := resource.New(
			context.Background(),
			resource.WithSchemaURL(otelsemconv.SchemaURL),
			resource.WithAttributes(otelsemconv.ServiceNameKey.String(svc)),
			resource.WithTelemetrySDK(),
			resource.WithHost(),
			resource.WithOSType(),
		)
		if err != nil {
			logger.Sugar().Fatalf("resource creation failed: %s", err)
		}
		var opts []sdktrace.TracerProviderOption
		if !cfg.Static {
			opts = []sdktrace.TracerProviderOption{
				sdktrace.WithBatcher(exp, sdktrace.WithBlocking()),
				sdktrace.WithResource(res),
			}
		} else {
			opts = []sdktrace.TracerProviderOption{
				sdktrace.WithBatcher(exp, sdktrace.WithBlocking()),
				sdktrace.WithResource(res),
				sdktrace.WithIDGenerator(NewPseudoRandomIDGenerator(countG)),
			}
		}
		if flagAdaptiveSamplingEndpoint != "" {
			jaegerRemoteSampler := jaegerremote.New(
				svc,
				jaegerremote.WithSamplingServerURL(flagAdaptiveSamplingEndpoint),
				jaegerremote.WithSamplingRefreshInterval(5*time.Second),
				jaegerremote.WithInitialSampler(sdktrace.TraceIDRatioBased(0.5)),
			)
			opts = append(opts, sdktrace.WithSampler(jaegerRemoteSampler))
			logger.Sugar().Infof("using adaptive sampling URL: %s", flagAdaptiveSamplingEndpoint)
		}
		tp := sdktrace.NewTracerProvider(opts...)
		tracers = append(tracers, tp.Tracer(cfg.Service))
		shutdown = append(shutdown, tp.Shutdown)
	}
	return tracers, func(ctx context.Context) error {
		var errs []error
		for _, f := range shutdown {
			errs = append(errs, f(ctx))
		}
		return errors.Join(errs...)
	}
}

func createOtelExporter(exporterType string) (sdktrace.SpanExporter, error) {
	var exporter sdktrace.SpanExporter
	var err error
	switch exporterType {
	case "jaeger":
		return nil, errors.New("jaeger exporter is no longer supported, please use otlp")
	case "otlp", "otlp-http":
		client := otlptracehttp.NewClient(
			otlptracehttp.WithInsecure(),
		)
		exporter, err = otlptrace.New(context.Background(), client)
	case "otlp-grpc":
		client := otlptracegrpc.NewClient(
			otlptracegrpc.WithInsecure(),
		)
		exporter, err = otlptrace.New(context.Background(), client)
	case "stdout":
		exporter, err = stdouttrace.New()
	default:
		return nil, fmt.Errorf("unrecognized exporter type %s", exporterType)
	}
	return exporter, err
}

var countG int64 = 1

// PseudoRandomIDGenerator For static data
type PseudoRandomIDGenerator struct {
	rng *rand.Rand
}

// NewPseudoRandomIDGenerator TODO 第N个TracerProvider 种子为N / 应该改成可指定
func NewPseudoRandomIDGenerator(seed int64) sdktrace.IDGenerator {
	src := rand.NewSource(seed)
	countG++
	return &PseudoRandomIDGenerator{
		rng: rand.New(src),
	}
}

func (g *PseudoRandomIDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	tid := trace.TraceID{}
	sid := trace.SpanID{}

	for {
		binary.NativeEndian.PutUint64(tid[:8], g.rng.Uint64())
		binary.NativeEndian.PutUint64(tid[8:], g.rng.Uint64())
		if tid.IsValid() {
			break
		}
	}
	if !tid.IsValid() {
		tid[0] = 1
	}
	for {
		binary.NativeEndian.PutUint64(sid[:], g.rng.Uint64())
		if sid.IsValid() {
			break
		}
	}
	if !sid.IsValid() {
		sid[0] = 1
	}

	return tid, sid
}

func (g *PseudoRandomIDGenerator) NewSpanID(ctx context.Context, traceID trace.TraceID) trace.SpanID {
	sid := trace.SpanID{}
	for {
		binary.NativeEndian.PutUint64(sid[:], g.rng.Uint64())
		if sid.IsValid() {
			break
		}
	}
	if !sid.IsValid() {
		sid[0] = 1
	}
	return sid
}
