package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"

	bredis "github.com/kalbhor/tasqueue/v2/brokers/redis"
	rredis "github.com/kalbhor/tasqueue/v2/results/redis"

	"github.com/go-chi/chi/v5"
	"github.com/knadh/koanf/v2"
	"github.com/zerodha/dungbeetle/internal/core"
	"github.com/zerodha/dungbeetle/internal/dbpool"
	"github.com/zerodha/dungbeetle/internal/resultbackends/sqldb"
)

var (
	//go:embed config.sample.toml
	efs embed.FS
)

func generateConfig() error {
	if _, err := os.Stat("config.toml"); !os.IsNotExist(err) {
		return errors.New("config.toml exists. Remove it to generate a new one")
	}

	// Generate config file.
	b, err := efs.ReadFile("config.sample.toml")
	if err != nil {
		return fmt.Errorf("error reading sample config: %v", err)
	}

	if err := os.WriteFile("config.toml", b, 0644); err != nil {
		return err
	}

	return nil
}

// initHTTP is a blocking function that initializes and runs the HTTP server.
func initHTTP(co *core.Core) {
	r := chi.NewRouter()

	// Middleware to attach the instance of core to every handler.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), "core", co)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	})

	// Register HTTP handlers.
	r.Get("/", func(w http.ResponseWriter, r *http.Request) {
		sendResponse(w, fmt.Sprintf("dungbeetle %s", buildString))
	})
	r.Get("/tasks", handleGetTasksList)
	r.Post("/tasks/{taskName}/jobs", handlePostJob)
	r.Get("/jobs/{jobID}", handleGetJobStatus)
	r.Delete("/jobs/{jobID}", handleCancelJob)
	r.Delete("/groups/{groupID}", handleCancelGroupJob)
	r.Get("/jobs/queue/{queue}", handleGetPendingJobs)
	r.Post("/groups", handlePostJobGroup)
	r.Get("/groups/{groupID}", handleGetGroupStatus)

	lo.Info("starting HTTP server", "address", ko.String("server"))
	if err := http.ListenAndServe(ko.String("server"), r); err != nil {
		lo.Error("shutting down http server", "error", err)
	}
	os.Exit(0)
}

func initCore(ko *koanf.Koanf) (*core.Core, error) {
	// Source DBs config.
	var srcDBs map[string]dbpool.Config
	if err := ko.Unmarshal("db", &srcDBs); err != nil {
		lo.Error("error reading source DB config", "error", err)
		return nil, fmt.Errorf("error reading source DB config : %w", err)
	}
	if len(srcDBs) == 0 {
		lo.Error("found 0 source databases in config")
		return nil, fmt.Errorf("found 0 source databases in config")
	}

	// Result DBs config.
	var resDBs map[string]dbpool.Config
	if err := ko.Unmarshal("results", &resDBs); err != nil {
		return nil, fmt.Errorf("error reading source DB config: %w", err)
	}
	if len(resDBs) == 0 {
		return nil, fmt.Errorf("found 0 result backends in config")
	}

	// Connect to source DBs.
	srcPool, err := dbpool.New(srcDBs)
	if err != nil {
		return nil, err
	}

	// Connect to result DBs.
	resPool, err := dbpool.New(resDBs)
	if err != nil {
		return nil, err
	}

	// Initialize the result backend controller for every backend.
	backends := make(core.ResultBackends)
	for name, db := range resPool {
		opt := sqldb.Opt{
			DBType:         resDBs[name].Type,
			ResultsTable:   ko.MustString(fmt.Sprintf("results.%s.results_table", name)),
			UnloggedTables: resDBs[name].Unlogged,
		}

		backend, err := sqldb.NewSQLBackend(db, opt, lo)
		if err != nil {
			return nil, fmt.Errorf("error initializing result backend: %w", err)
		}

		backends[name] = backend
	}

	if v := ko.MustString("job_queue.broker.type"); v != "redis" {
		return nil, fmt.Errorf("unsupported job_queue.broker.type '%s'. Only 'redis' is supported.", v)
	}
	if v := ko.MustString("job_queue.state.type"); v != "redis" {
		return nil, fmt.Errorf("unsupported job_queue.state.type '%s'. Only 'redis' is supported.", v)
	}

	lo := slog.Default()
	rBroker := bredis.New(bredis.Options{
		PollPeriod:   bredis.DefaultPollPeriod,
		Addrs:        ko.MustStrings("job_queue.broker.addresses"),
		Password:     ko.String("job_queue.broker.password"),
		DB:           ko.Int("job_queue.broker.db"),
		MinIdleConns: ko.MustInt("job_queue.broker.max_idle"),
		DialTimeout:  ko.MustDuration("job_queue.broker.dial_timeout"),
		ReadTimeout:  ko.MustDuration("job_queue.broker.read_timeout"),
		WriteTimeout: ko.MustDuration("job_queue.broker.write_timeout"),
	}, lo)

	rResult := rredis.New(rredis.Options{
		Addrs:        ko.MustStrings("job_queue.state.addresses"),
		Password:     ko.String("job_queue.state.password"),
		DB:           ko.Int("job_queue.state.db"),
		MinIdleConns: ko.MustInt("job_queue.state.max_idle"),
		DialTimeout:  ko.MustDuration("job_queue.state.dial_timeout"),
		ReadTimeout:  ko.MustDuration("job_queue.state.read_timeout"),
		WriteTimeout: ko.MustDuration("job_queue.state.write_timeout"),
		Expiry:       ko.Duration("job_queue.state.expiry"),
		MetaExpiry:   ko.Duration("job_queue.state.meta_expiry"),
	}, lo)

	// Initialize the server and load SQL tasks.
	co := core.New(core.Opt{
		DefaultQueue:            ko.MustString("queue"),
		DefaultGroupConcurrency: ko.MustInt("worker-concurrency"),
		DefaultJobTTL:           ko.MustDuration("app.job_ttl"),
		Results:                 rResult,
		Broker:                  rBroker,
	}, srcPool, backends, lo)
	if err := co.LoadTasks(ko.MustStrings("sql-directory")); err != nil {
		return nil, fmt.Errorf("error loading tasks : %w", err)
	}

	return co, nil
}
