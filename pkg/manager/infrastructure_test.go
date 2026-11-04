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

package manager

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

func TestOnceGuardedRunsOnce(t *testing.T) {
	var calls atomic.Int32
	fn := onceGuarded(func() error {
		calls.Add(1)
		return nil
	})

	for i := range 3 {
		if err := fn(); err != nil {
			t.Fatalf("unexpected error on call %d: %v", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times, want 1", calls.Load())
	}
}

func TestOnceGuardedStickyError(t *testing.T) {
	sentinel := errors.New("registration failed")
	var calls atomic.Int32
	fn := onceGuarded(func() error {
		calls.Add(1)
		return sentinel
	})

	for i := range 3 {
		if err := fn(); !errors.Is(err, sentinel) {
			t.Fatalf("call %d returned %v, want the sticky sentinel error", i, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times, want 1", calls.Load())
	}
}

func TestOnceGuardedConcurrentCallsRunOnce(t *testing.T) {
	var calls atomic.Int32
	fn := onceGuarded(func() error {
		calls.Add(1)
		return nil
	})

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := fn(); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if calls.Load() != 1 {
		t.Fatalf("fn ran %d times under concurrency, want 1", calls.Load())
	}
}
