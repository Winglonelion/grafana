// Package eval executes the condition for an alert definition, evaluates the condition results, and
// returns the alert instance states.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/data"
	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/components/simplejson"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/plugins"
	"github.com/grafana/grafana/pkg/registry"
	"github.com/grafana/grafana/pkg/services/datasources"
	"github.com/grafana/grafana/pkg/tsdb"
)

type minimalDashboard struct {
	Panels []struct {
		ID         int64              `json:"id"`
		Datasource string             `json:"datasource"`
		Targets    []*simplejson.Json `json:"targets"`
	} `json:"panels"`
}

// AlertNG is the service for evaluating the condition of an alert definition.
type AlertNG struct {
	DatasourceCache datasources.CacheService `inject:""`
}

func init() {
	registry.RegisterService(&AlertNG{})
}

// Init initializes the AlertingService.
func (ng *AlertNG) Init() error {
	return nil
}

// AlertExecCtx is the context provided for executing an alert condition.
type AlertExecCtx struct {
	AlertDefitionID int64
	SignedInUser    *models.SignedInUser

	Ctx context.Context
}

// Condition contains backend expressions and queries and the RefID
// of the query or expression that will be evaluated.
type Condition struct {
	RefID string `json:"refId"`

	QueriesAndExpressions []tsdb.Query `json:"queriesAndExpressions"`
}

// ExecutionResults contains the unevaluated results from executing
// a condition.
type ExecutionResults struct {
	AlertDefinitionID int64

	Error error

	Results data.Frames
}

// Results is a slice of evaluated alert instances states.
type Results []Result

// Result contains the evaluated state of an alert instance
// identified by its labels.
type Result struct {
	Instance data.Labels
	State    State // Enum
}

// State is an enum of the evaluation state for an alert instance.
type State int

const (
	// Normal is the eval state for an alert instance condition
	// that evaluated to false.
	Normal State = iota

	// Alerting is the eval state for an alert instance condition
	// that evaluated to false.
	Alerting
)

func (s State) String() string {
	return [...]string{"Normal", "Alerting"}[s]
}

// IsValid checks the conditions validity
func (c Condition) IsValid() bool {
	// TODO search for refIDs in QueriesAndExpressions
	return len(c.QueriesAndExpressions) != 0
}

// LoadAlertCondition returns a Condition object for the given alertDefintionId.
func (ng *AlertNG) LoadAlertCondition(dashboardID int64, panelID int64, conditionRefID string, signedInUser *models.SignedInUser, skipCache bool) (*Condition, error) {
	// get queries from the dashboard (because GEL expressions cannot be stored in alerts so far)
	getDashboardQuery := models.GetDashboardQuery{Id: dashboardID}
	if err := bus.Dispatch(&getDashboardQuery); err != nil {
		return nil, err
	}

	blob, err := getDashboardQuery.Result.Data.MarshalJSON()
	if err != nil {
		return nil, errors.New("Failed to marshal dashboard JSON")
	}
	var dash minimalDashboard
	err = json.Unmarshal(blob, &dash)
	if err != nil {
		return nil, errors.New("Failed to unmarshal dashboard JSON")
	}

	condition := Condition{}
	for _, p := range dash.Panels {
		if p.ID == panelID {
			panelDatasource := p.Datasource
			var ds *models.DataSource
			for i, query := range p.Targets {
				refID := query.Get("refId").MustString("A")
				queryDatasource := query.Get("datasource").MustString()

				if i == 0 && queryDatasource != "__expr__" {
					dsName := panelDatasource
					if queryDatasource != "" {
						dsName = queryDatasource
					}

					getDataSourceByNameQuery := models.GetDataSourceByNameQuery{Name: dsName, OrgId: getDashboardQuery.Result.OrgId}
					if err := bus.Dispatch(&getDataSourceByNameQuery); err != nil {
						return nil, err
					}

					ds, err = ng.DatasourceCache.GetDatasource(getDataSourceByNameQuery.Result.Id, signedInUser, skipCache)
					if err != nil {
						return nil, err
					}
				}

				if ds == nil {
					return nil, errors.New("No datasource reference found")
				}

				if queryDatasource == "" {
					query.Set("datasource", ds.Name)
				}

				if query.Get("datasourceId").MustString() == "" {
					query.Set("datasourceId", ds.Id)
				}

				if query.Get("orgId").MustString() == "" { // GEL requires orgID inside the query JSON
					// need to decide which organization id is expected there
					// in grafana queries is passed the signed in user organization id:
					// https://github.com/grafana/grafana/blob/34a355fe542b511ed02976523aa6716aeb00bde6/packages/grafana-runtime/src/utils/DataSourceWithBackend.ts#L60
					// but I think that it should be datasource org id instead
					query.Set("orgId", 0)
				}

				if query.Get("maxDataPoints").MustString() == "" { // GEL requires maxDataPoints inside the query JSON
					query.Set("maxDataPoints", 100)
				}

				// intervalMS is calculated by the frontend
				// should we do something similar?
				if query.Get("intervalMs").MustString() == "" { // GEL requires intervalMs inside the query JSON
					query.Set("intervalMs", 1000)
				}

				condition.QueriesAndExpressions = append(condition.QueriesAndExpressions, tsdb.Query{
					RefId:         refID,
					MaxDataPoints: query.Get("maxDataPoints").MustInt64(100),
					IntervalMs:    query.Get("intervalMs").MustInt64(1000),
					QueryType:     query.Get("queryType").MustString(""),
					Model:         query,
					DataSource:    ds,
				})
			}
		}
	}
	condition.RefID = conditionRefID
	return &condition, nil
}

// Execute runs the Condition's expressions or queries.
func (c *Condition) Execute(ctx AlertExecCtx, fromStr, toStr string) (*ExecutionResults, error) {
	result := ExecutionResults{}
	if !c.IsValid() {
		return nil, fmt.Errorf("Invalid conditions")
	}

	request := &tsdb.TsdbQuery{
		TimeRange: tsdb.NewTimeRange(fromStr, toStr),
		Debug:     true,
		User:      ctx.SignedInUser,
	}
	for i := range c.QueriesAndExpressions {
		request.Queries = append(request.Queries, &c.QueriesAndExpressions[i])
	}

	resp, err := plugins.Transform.Transform(ctx.Ctx, request)
	if err != nil {
		result.Error = err
		return &result, err
	}

	conditionResult := resp.Results[c.RefID]
	if conditionResult == nil {
		err = fmt.Errorf("No GEL results")
		result.Error = err
		return &result, err
	}

	result.Results, err = conditionResult.Dataframes.Decoded()
	if err != nil {
		result.Error = err
		return &result, err
	}

	return &result, nil
}

// EvaluateExecutionResult takes the ExecutionResult, and returns a frame where
// each column is a string type that holds a string representing its state.
func EvaluateExecutionResult(results *ExecutionResults) (Results, error) {
	evalResults := make([]Result, 0)
	labels := make(map[string]bool)
	for _, f := range results.Results {
		rowLen, err := f.RowLen()
		if err != nil {
			return nil, fmt.Errorf("Unable to get frame row length")
		}
		if rowLen > 1 {
			return nil, fmt.Errorf("Invalid frame %v: row length %v", f.Name, rowLen)
		}

		if len(f.Fields) > 1 {
			return nil, fmt.Errorf("Invalid frame %v: field length %v", f.Name, len(f.Fields))
		}

		if f.Fields[0].Type() != data.FieldTypeNullableFloat64 {
			return nil, fmt.Errorf("Invalid frame %v: field type %v", f.Name, f.Fields[0].Type())
		}

		labelsStr := f.Fields[0].Labels.String()
		_, ok := labels[labelsStr]
		if ok {
			return nil, fmt.Errorf("Invalid frame %v: frames cannot uniquely be identified by its labels: %q", f.Name, labelsStr)
		}
		labels[labelsStr] = true

		state := Normal
		val, err := f.Fields[0].FloatAt(0)
		if err != nil || val != 0 {
			state = Alerting
		}

		evalResults = append(evalResults, Result{
			Instance: f.Fields[0].Labels,
			State:    state,
		})
	}
	return evalResults, nil
}

// AsDataFrame forms the EvalResults in Frame suitable for displaying in the table panel of the front end.
// This may be temporary, as there might be a fair amount we want to display in the frontend, and it might not make sense to store that in data.Frame.
// For the first pass, I would expect a Frame with a single row, and a column for each instance with a boolean value.
func (evalResults Results) AsDataFrame() data.Frame {
	fields := make([]*data.Field, 0)
	for _, evalResult := range evalResults {
		fields = append(fields, data.NewField("", evalResult.Instance, []bool{evalResult.State != Normal}))
	}
	f := data.NewFrame("", fields...)
	return *f
}
