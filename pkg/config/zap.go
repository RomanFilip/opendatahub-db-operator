/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package config

import (
	"fmt"
	"strings"

	"github.com/go-logr/logr"
	"go.uber.org/zap/zapcore"
	ctrlzap "sigs.k8s.io/controller-runtime/pkg/log/zap"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
)

func (c ZapConfig) NewLogger() (logr.Logger, error) {
	level, err := zapcore.ParseLevel(c.Level)
	if err != nil {
		return logr.Logger{}, fmt.Errorf("parsing zap level %q: %w", c.Level, err)
	}

	opts := []ctrlzap.Opts{
		ctrlzap.UseDevMode(c.DevMode),
		ctrlzap.Level(level),
	}

	switch strings.ToLower(c.Encoder) {
	case "":
		// Use controller-runtime defaults: console in dev mode, JSON otherwise.
	case "console":
		opts = append(opts, ctrlzap.ConsoleEncoder())
	case "json":
		opts = append(opts, ctrlzap.JSONEncoder())
	default:
		return logr.Logger{}, fmt.Errorf("unsupported zap encoder %q", c.Encoder)
	}

	return ctrlzap.New(opts...), nil
}

// TraceQueries reports whether pgx should trace every database statement.
// Statement-level output is the diagnostic detail debug level asks for; at the
// default info level the operator traces database errors only.
func (c ZapConfig) TraceQueries() bool {
	level, err := zapcore.ParseLevel(c.Level)
	return err == nil && level == zapcore.DebugLevel
}

// WithPostgresTracing returns poolCfg carrying the operator logger and the pgx
// trace level derived from it, so every pool the operator opens logs through
// the configured zap logger at the severity the configured level asks for.
func (c *Config) WithPostgresTracing(poolCfg postgres.Config) postgres.Config {
	if c == nil {
		return poolCfg
	}
	poolCfg.Logger = c.Logger
	poolCfg.TraceQueries = c.Controller.Zap.TraceQueries()
	return poolCfg
}
