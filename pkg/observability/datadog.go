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

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/DataDog/datadog-api-client-go/v2/api/datadog"
	"github.com/DataDog/datadog-api-client-go/v2/api/datadogV1"
)

// monitorPrefix is prepended to every monitor name managed by this operator.
// It is also used as the search anchor in DeleteMonitors so we never
// accidentally touch monitors created outside of k-sentinel.
const monitorPrefix = "[k-sentinel]"

// DatadogClient implements MonitorClient against the Datadog Monitors v1 API.
// Credentials are read from DD_API_KEY and DD_APP_KEY at construction time.
type DatadogClient struct {
	api *datadogV1.MonitorsApi
}

// compile-time assertion: DatadogClient must satisfy MonitorClient.
var _ MonitorClient = (*DatadogClient)(nil)

// NewDatadogClient validates that DD_API_KEY and DD_APP_KEY are present in the
// environment and returns a ready-to-use DatadogClient. It fails fast at
// startup rather than at the first API call.
func NewDatadogClient() (*DatadogClient, error) {
	if os.Getenv("DD_API_KEY") == "" || os.Getenv("DD_APP_KEY") == "" {
		return nil, fmt.Errorf("DD_API_KEY and DD_APP_KEY environment variables are required")
	}
	apiClient := datadog.NewAPIClient(datadog.NewConfiguration())
	return &DatadogClient{api: datadogV1.NewMonitorsApi(apiClient)}, nil
}

// CreateMonitors provisions the standard monitor set for appName.
//
// Idempotency is guaranteed at the individual monitor level: before each
// CreateMonitor call we search for a monitor with the exact same name. If one
// already exists it is left untouched. This means a partial failure (e.g. the
// process crashes after creating the first of two monitors) is safe to retry
// — only the missing monitors will be created on the next reconciliation.
func (c *DatadogClient) CreateMonitors(ctx context.Context, appName string, team string) error {
	// datadog.NewDefaultContext wraps ctx adding DD_API_KEY / DD_APP_KEY from
	// env. We derive it from the incoming ctx so cancellation propagates.
	ddCtx := datadog.NewDefaultContext(ctx)

	for _, m := range buildMonitors(appName, team) {
		if err := c.createIfNotExists(ddCtx, m); err != nil {
			return err
		}
	}
	return nil
}

// DeleteMonitors finds every monitor whose name begins with the k-sentinel
// prefix for appName and deletes it. The prefix filter ensures we never touch
// monitors created outside this operator.
func (c *DatadogClient) DeleteMonitors(ctx context.Context, appName string) error {
	ddCtx := datadog.NewDefaultContext(ctx)
	prefix := namePrefix(appName)

	// Datadog's name filter is a substring match, so we do an exact prefix
	// check in Go after fetching to avoid false positives (e.g. "api" matching
	// "api-gateway").
	monitors, _, err := c.api.ListMonitors(ddCtx,
		*datadogV1.NewListMonitorsOptionalParameters().WithName(prefix))
	if err != nil {
		return fmt.Errorf("listing monitors for %q: %w", appName, err)
	}

	for _, m := range monitors {
		if !strings.HasPrefix(m.GetName(), prefix) {
			continue
		}
		if _, _, err := c.api.DeleteMonitor(ddCtx, m.GetId()); err != nil {
			return fmt.Errorf("deleting monitor %d (%q): %w", m.GetId(), m.GetName(), err)
		}
	}
	return nil
}

// createIfNotExists issues a CreateMonitor only when no monitor with the same
// name already exists in the account. The exact-name check guards against the
// substring behaviour of Datadog's list API.
func (c *DatadogClient) createIfNotExists(ddCtx context.Context, monitor datadogV1.Monitor) error {
	name := monitor.GetName()

	existing, _, err := c.api.ListMonitors(ddCtx,
		*datadogV1.NewListMonitorsOptionalParameters().WithName(name))
	if err != nil {
		return fmt.Errorf("searching for monitor %q: %w", name, err)
	}
	for _, m := range existing {
		if m.GetName() == name {
			return nil // already provisioned — nothing to do
		}
	}

	if _, _, err := c.api.CreateMonitor(ddCtx, monitor); err != nil {
		return fmt.Errorf("creating monitor %q: %w", name, err)
	}
	return nil
}

// buildMonitors returns the standard monitor set for a Deployment.
// Add entries here to roll out new alert types to all managed services.
func buildMonitors(appName, team string) []datadogV1.Monitor {
	tags := []string{
		"managed-by:k-sentinel",
		fmt.Sprintf("kube_deployment:%s", appName),
		fmt.Sprintf("team:%s", team),
	}

	return []datadogV1.Monitor{
		buildRestartMonitor(appName, team, tags),
	}
}

// buildRestartMonitor creates a metric alert that fires when the pod restart
// count exceeds the threshold — the primary signal for CrashLoopBackOff.
func buildRestartMonitor(appName, team string, tags []string) datadogV1.Monitor {
	query := fmt.Sprintf(
		"max(last_5m):sum:kubernetes.containers.restarts{kube_deployment:%s} by {pod_name} > 5",
		appName,
	)
	message := fmt.Sprintf(
		"Pod restart rate is critically high for deployment **%s** (team: %s).\n\n"+
			"Likely cause: CrashLoopBackOff or OOMKill. Check `kubectl describe pod` and recent logs.\n\n"+
			"@team-%s",
		appName, team, team,
	)

	critical := 5.0
	warning := 3.0

	options := datadogV1.NewMonitorOptions()
	options.SetNotifyNoData(false)
	options.SetRequireFullWindow(false)
	// Critical is *float64; Warning is NullableFloat64 (can be absent/null).
	options.SetThresholds(datadogV1.MonitorThresholds{
		Critical: &critical,
		Warning:  *datadog.NewNullableFloat64(&warning),
	})

	m := datadogV1.NewMonitor(query, datadogV1.MONITORTYPE_METRIC_ALERT)
	m.SetName(namePrefix(appName) + " - Pod Restart Rate High")
	m.SetMessage(message)
	m.SetTags(tags)
	m.SetOptions(*options)

	return *m
}

// namePrefix returns the search anchor used for both idempotency checks and
// deletion. Format: "[k-sentinel] <appName>".
func namePrefix(appName string) string {
	return fmt.Sprintf("%s %s", monitorPrefix, appName)
}
