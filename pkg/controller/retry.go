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

package controller

import (
	"time"

	odherrors "github.com/opendatahub-io/odh-platform-utilities/framework/controller/actions/errors"

	"github.com/opendatahub-io/opendatahub-db-operator/pkg/postgres"
)

const connectionRefusedRetryAfter = time.Second

// StopWithQuickRetryIfConnectionRefused returns a quick-requeue StopError when
// err is a connection-refused failure, and nil otherwise.
//
// Detection asks the PostgreSQL boundary for the retry class it established
// from the raw driver error, because a Client never returns the *net.OpError
// chain that classification needs — it returns the projected SafeError that
// carries the class. Errors that never crossed that boundary are classified
// from their own chain.
func StopWithQuickRetryIfConnectionRefused(err error) error {
	if err == nil {
		return nil
	}

	if postgres.RetryClassOf(err) == postgres.RetryQuick {
		return odherrors.NewStopErrorW(err).WithRequeueAfter(connectionRefusedRetryAfter)
	}

	return nil
}
