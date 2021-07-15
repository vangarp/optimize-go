/*
Copyright 2021 GramLabs, Inc.

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

package v2_test

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"flag"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thestormforge/optimize-go/pkg/api"
	applications "github.com/thestormforge/optimize-go/pkg/api/applications/v2"
	experiments "github.com/thestormforge/optimize-go/pkg/api/experiments/v1alpha1"
	"github.com/thestormforge/optimize-go/pkg/api/internal/apitest"
)

var (
	client api.Client
	cases  []apitest.ApplicationTestDefinition
)

func TestMain(m *testing.M) {
	var err error
	path := "testdata"
	flag.Parse()

	// Create a client
	client, err = apitest.NewClient(context.TODO())
	if err != nil {
		log.Fatal(err)
	}

	// Load the test data
	cases, err = apitest.ReadApplicationTestData(path)
	if err != nil {
		log.Fatal(err)
	}

	// Execute the tests
	os.Exit(m.Run())
}

func TestAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping API test in short mode.")
	}

	appAPI := applications.NewAPI(client)

	for i := range cases {
		t.Run(cases[i].Application.DisplayName, func(t *testing.T) {
			runTest(t, &cases[i], appAPI)
		})
	}
}

func runTest(t *testing.T, td *apitest.ApplicationTestDefinition, appAPI applications.API) {
	ctx := context.Background()
	var appMeta, scnMeta api.Metadata
	var run sync.WaitGroup
	subCtx, cancelSub := context.WithCancel(ctx)
	activity := make(chan applications.ActivityItem)

	t.Run("Subscribe", func(t *testing.T) {
		q := applications.ActivityFeedQuery{}
		q.SetType(applications.TagScan, applications.TagRun)
		sub, err := appAPI.SubscribeActivity(subCtx, q)
		if assert.NoError(t, err, "failed to create activity subscriber") {
			sub.Subscribe(ctx, activity)
		} else {
			close(activity)
		}
	})

	ok := t.Run("Create Application", func(t *testing.T) {
		var err error
		appMeta, err = appAPI.CreateApplication(ctx, td.Application)
		require.NoError(t, err, "failed to create application")
		assert.NotEmpty(t, appMeta.Location(), "missing location")
		assert.NotEmpty(t, appMeta.Link(api.RelationScenarios), "missing scenarios link")
		assert.Equal(t, td.Application.DisplayName, appMeta.Title(), "title metadata does not match")
	})

	ok = t.Run("Create Scenario", func(t *testing.T) {
		if !ok {
			t.Skip("skipping scenario.")
		}

		var err error
		scnMeta, err = appAPI.CreateScenario(ctx, appMeta.Link(api.RelationScenarios), td.Scenario)
		require.NoError(t, err, "failed to create scenario")
		assert.NotEmpty(t, scnMeta.Location(), "missing location")
		assert.NotEmpty(t, scnMeta.Link(api.RelationTemplate), "missing template link")
		assert.Equal(t, appMeta.Location(), scnMeta.Link(api.RelationUp), "application link does not match")
		assert.Equal(t, td.Scenario.DisplayName, scnMeta.Title(), "title metadata does not match")
	})

	t.Run("Handle Activity", func(t *testing.T) {
		t.Parallel()
		if !ok {
			t.Skip("skipping activity handling.")
		}

		for ai := range activity {
			// NOTE: We limited the activity types when we subscribed
			assert.True(t, ai.HasTag(applications.TagScan) || ai.HasTag(applications.TagRun), "unexpected item tag")

			// Both scan and run use the external URL to point at the scenario
			scn, err := appAPI.GetScenario(ctx, ai.ExternalURL)
			require.NoError(t, err, "failed to retrieve activity scenario")
			require.NotEmpty(t, scn.Link(api.RelationTemplate), "missing template link")
			require.NotEmpty(t, scn.Link(api.RelationExperiments), "missing experiments link")
			require.NotEmpty(t, scn.Link(api.RelationUp), "missing application link")

			app, err := appAPI.GetApplication(ctx, scn.Link(api.RelationUp))
			require.NoError(t, err, "failed to retrieve scenario application")

			switch {

			case ai.HasTag(applications.TagScan):
				t.Run("Scan", func(t *testing.T) {
					err = appAPI.UpdateTemplate(ctx, scn.Link(api.RelationTemplate), td.GenerateTemplate())
					require.NoError(t, err, "failed to update template")
				})

			case ai.HasTag(applications.TagRun):
				t.Run("Run", func(t *testing.T) {
					defer run.Done()

					// Discard the template for the test, the defined experiment is fixed
					_, err = appAPI.GetTemplate(ctx, scn.Link(api.RelationTemplate))
					require.NoError(t, err, "failed to retrieve scenario template")

					expAPI, err := experiments.NewAPIWithEndpoint(client, scn.Link(api.RelationExperiments))
					require.NoError(t, err, "failed to create experiment API for application")

					exp, err := expAPI.CreateExperimentByName(ctx, newExperimentName(), td.Experiment)
					require.NoError(t, err, "failed to create experiment")
					assert.NotEmpty(t, exp.Link(api.RelationTrials), "missing trials link")
					assert.NotEmpty(t, exp.Link(api.RelationNextTrial), "missing next trial link")
					assert.NotEmpty(t, exp.Link(api.RelationSelf), "missing self link")
					assert.Equal(t, app.Name, exp.Labels["application"], "incorrect application label")
					assert.Equal(t, scn.Name, exp.Labels["scenario"], "incorrect scenario label")

					_, err = expAPI.CreateTrial(ctx, exp.Link(api.RelationTrials), experiments.TrialAssignments{
						Labels:      map[string]string{"baseline": "true"},
						Assignments: td.Baseline,
					})
					require.NoError(t, err, "failed to create baseline trial")

					for {
						ta, err := expAPI.NextTrial(ctx, exp.Link(api.RelationNextTrial))
						var aerr *api.Error
						if errors.As(err, &aerr) && aerr.Type == experiments.ErrExperimentStopped {
							break
						}
						require.NoError(t, err, "failed to fetch trial assignments")
						assert.NotEmpty(t, ta.Location(), "missing location")

						err = expAPI.ReportTrial(ctx, ta.Location(), td.TrialResults(&ta))
						require.NoError(t, err, "failed to report trial")
					}
				})
			}

			err = appAPI.DeleteActivity(ctx, ai.URL)
			require.NoError(t, err, "failed to acknowledge activity")
		}
	})

	t.Run("Request Activity", func(t *testing.T) {
		if !ok {
			t.Skip("skipping activity request.")
		}

		// TODO This should use CheckEndpoint to get the feed link via a HEAD request
		lst, err := appAPI.ListApplications(ctx, applications.ApplicationListQuery{})
		require.NoError(t, err, "failed to fetch the application list necessary for the feed URL")

		// TODO Do we need to create this or is it implicit from create scenario?
		sa := &applications.ScanActivity{
			Scenario: scnMeta.Location(),
		}
		err = appAPI.CreateActivity(ctx, lst.Link(api.RelationAlternate), applications.Activity{Scan: sa})
		require.NoError(t, err, "failed to request scan")

		ra := &applications.RunActivity{
			Scenario: scnMeta.Location(),
		}
		err = appAPI.CreateActivity(ctx, lst.Link(api.RelationAlternate), applications.Activity{Run: ra})
		require.NoError(t, err, "failed to request run")
		run.Add(1)
	})

	// Wait for all run activities to finish
	run.Wait()
	cancelSub()

	t.Run("Delete Application", func(t *testing.T) {
		if appMeta.Location() == "" {
			t.Skip("skipping delete application.")
		}

		err := appAPI.DeleteApplication(ctx, appMeta.Location())
		require.NoError(t, err, "failed to delete application")

		// TODO Verify the experiment still exists via the /v1/experiments/ endpoint? Or will it be deleted?
	})
}

// newExperimentName returns a random experiment name. For consistency with the
// backend you would normally want to use a ULID here. To avoid introducing an
// explicit dependencies for testing, we are just using something that looks ULID-ish.
func newExperimentName() experiments.ExperimentName {
	var name [16]byte
	binary.BigEndian.PutUint64(name[:], uint64(time.Now().UTC().UnixNano()/int64(time.Millisecond))<<16)
	_, _ = rand.Read(name[6:])

	var notCrockford = base32.
		NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").
		WithPadding(base32.NoPadding)
	return experiments.ExperimentName(notCrockford.EncodeToString(name[:]))
}