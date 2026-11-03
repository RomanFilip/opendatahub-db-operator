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

package controller_test

import (
	"errors"
	"fmt"
	"net"
	"syscall"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/controller"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
	"github.com/opendatahub-io/opendatahub-db-operator/pkg/secret"
)

// connectionRefusedErr is the raw driver shape a restarting PostgreSQL produces.
func connectionRefusedErr() error {
	return fmt.Errorf("connecting: %w", &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: fmt.Errorf("connect: %w", syscall.ECONNREFUSED),
	})
}

// TestStopWithQuickRetryIfConnectionRefused_SurvivesProjection is the behavior a
// Client boundary must not lose: the projected error no longer carries the
// *net.OpError chain, so the retry class has to come from the projection.
func TestStopWithQuickRetryIfConnectionRefused_SurvivesProjection(t *testing.T) {
	g := NewWithT(t)

	projected := postgres.ProjectError("ping", connectionRefusedErr(), secret.FromString("secret"))
	g.Expect(projected.Error()).To(ContainSubstring("connection refused"), "diagnostics survive")

	for _, err := range []error{connectionRefusedErr(), projected, fmt.Errorf("ensuring role: %w", projected)} {
		retryErr := controller.StopWithQuickRetryIfConnectionRefused(err)
		g.Expect(retryErr).To(HaveOccurred())

		var stop odherrors.StopError
		g.Expect(errors.As(retryErr, &stop)).To(BeTrue())
		g.Expect(stop.RequeueAfter()).To(Equal(time.Second))
	}
}

func TestStopWithQuickRetryIfConnectionRefused_IgnoresOtherFailures(t *testing.T) {
	g := NewWithT(t)

	for _, err := range []error{
		nil,
		postgres.ProjectError("executing statement", errors.New("relation does not exist")),
		errors.New("a plain failure"),
	} {
		g.Expect(controller.StopWithQuickRetryIfConnectionRefused(err)).To(Succeed())
	}
}
