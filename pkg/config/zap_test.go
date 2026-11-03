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

package config_test

import (
	"testing"

	"github.com/go-logr/logr/funcr"
	. "github.com/onsi/gomega"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/config"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
)

func TestZapConfigTraceQueries(t *testing.T) {
	cases := []struct {
		name  string
		level string
		want  bool
	}{
		{name: "default-info-level-traces-errors-only", level: config.DefaultZapLevel, want: false},
		{name: "debug-level-traces-every-statement", level: "debug", want: true},
		{name: "invalid-level-traces-errors-only", level: "verbose", want: false},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			g := NewWithT(t)
			g.Expect(config.ZapConfig{Level: testCase.level}.TraceQueries()).To(Equal(testCase.want))
		})
	}
}

func TestConfigWithPostgresTracing(t *testing.T) {
	g := NewWithT(t)
	logger := funcr.New(func(string, string) {}, funcr.Options{})
	cfg := &config.Config{
		Controller: config.ControllerConfig{Zap: config.ZapConfig{Level: "debug"}},
		Logger:     logger,
	}

	poolCfg := cfg.WithPostgresTracing(postgres.Config{DBName: "db", SSLMode: postgres.SSLModeDisable})
	g.Expect(poolCfg.Logger).To(Equal(logger))
	g.Expect(poolCfg.TraceQueries).To(BeTrue())
	g.Expect(poolCfg.DBName).To(Equal("db"))
	g.Expect(poolCfg.SSLMode).To(Equal(postgres.SSLModeDisable))

	cfg.Controller.Zap.Level = config.DefaultZapLevel
	g.Expect(cfg.WithPostgresTracing(postgres.Config{}).TraceQueries).To(BeFalse())
}

func TestConfigWithPostgresTracing_NilConfig(t *testing.T) {
	g := NewWithT(t)
	var cfg *config.Config

	g.Expect(cfg.WithPostgresTracing(postgres.Config{DBName: "db"})).To(Equal(postgres.Config{DBName: "db"}))
}
