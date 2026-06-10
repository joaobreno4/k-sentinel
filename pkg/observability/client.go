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

package observability

import "context"

// MonitorClient abstracts the observability platform so the controller
// remains decoupled from any specific provider. Swap the concrete
// implementation in cmd/main.go to target a different backend.
type MonitorClient interface {
	// CreateMonitors provisions the standard monitor set for appName owned by team.
	// Implementations MUST be idempotent: calling this multiple times for the
	// same appName must not create duplicate monitors.
	CreateMonitors(ctx context.Context, appName string, team string) error

	// DeleteMonitors removes all monitors associated with appName.
	// It is safe to call even if no monitors currently exist.
	DeleteMonitors(ctx context.Context, appName string) error
}
