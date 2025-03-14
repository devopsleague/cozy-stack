package jobs

import (
	"strings"
	"testing"
	"time"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/tests/testutils"
	"github.com/cozy/cozy-stack/web/errors"
	"github.com/cozy/cozy-stack/web/middlewares"
	"github.com/gavv/httpexpect/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJobs(t *testing.T) {
	if testing.Short() {
		t.Skip("an instance is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	testutils.NeedCouchdb(t)
	setup := testutils.NewSetup(t, t.Name())

	job.AddWorker(&job.WorkerConfig{
		WorkerType:  "print",
		Concurrency: 4,
		WorkerFunc: func(ctx *job.WorkerContext) error {
			var msg string
			if err := ctx.UnmarshalMessage(&msg); err != nil {
				return err
			}

			t.Log(msg)

			return nil
		},
	})

	testInstance := setup.GetTestInstance()

	scope := strings.Join([]string{
		consts.Jobs + ":ALL:print:worker",
		consts.Triggers + ":ALL:print:worker",
	}, " ")
	token, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", scope,
		"", time.Now())

	ts := setup.GetTestServer("/jobs", Routes, func(r *echo.Echo) *echo.Echo {
		r.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
			return func(c echo.Context) error {
				tok := middlewares.GetRequestToken(c)
				// Forcing the token parsing to have the "claims" parameter in the
				// context (in production, it is done via
				// middlewares.CheckInstanceBlocked)
				_, err := middlewares.ParseJWT(c, testInstance, tok)
				if err != nil {
					return err
				}
				return next(c)
			}
		})
		return r
	})
	ts.Config.Handler.(*echo.Echo).HTTPErrorHandler = errors.ErrorHandler
	t.Cleanup(ts.Close)

	t.Run("GetQueue", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.GET("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			Expect().Status(200).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object().Value("data").Array().
			Length().Equal(0)
	})

	t.Run("CreateJob", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "attributes": { "arguments": "foobar" }
        }
      }`)).
			Expect().Status(202).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("worker", "print")
		attrs.NotContainsKey("manual_execution")
	})

	t.Run("CreateJobWithTimeout", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "attributes": {
		    "arguments": "foobar",
		    "options": { "timeout": 42 }
		  }
        }
      }`)).
			Expect().Status(202).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		jobID := obj.Path("$.data.id").String().NotEmpty().Raw()
		job, err := job.Get(testInstance, jobID)
		require.NoError(t, err)
		require.NotNil(t, job.Options)
		assert.Equal(t, 42*time.Second, job.Options.Timeout)
	})

	t.Run("CreateManualJob", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		obj := e.POST("/jobs/queue/print").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "arguments": "foobar",
            "manual": true
          }
        }
      }`)).
			Expect().Status(202).
			JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
			Object()

		attrs := obj.Path("$.data.attributes").Object()
		attrs.ValueEqual("worker", "print")
		attrs.ValueEqual("manual_execution", true)
	})

	t.Run("CreateJobForReservedWorker", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		e.POST("/jobs/queue/trash-files").
			WithHeader("Authorization", "Bearer "+token).
			WithHeader("Content-Type", "application/json").
			WithBytes([]byte(`{"data": {"attributes": {"arguments": "foobar"}}}`)).
			Expect().Status(403)
	})

	t.Run("CreateJobNotExist", func(t *testing.T) {
		e := testutils.CreateTestClient(t, ts.URL)

		tokenNone, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI",
			consts.Jobs+":ALL:none:worker",
			"", time.Now())

		e.POST("/jobs/queue/none"). // invalid
						WithHeader("Authorization", "Bearer "+tokenNone).
						WithHeader("Content-Type", "application/json").
						WithBytes([]byte(`{"data": {"attributes": {"arguments": "foobar"}}}`)).
						Expect().Status(404)
	})

	t.Run("AddGetAndDeleteTriggerAt", func(t *testing.T) {
		var triggerID string
		at := time.Now().Add(1100 * time.Millisecond).Format(time.RFC3339)

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@at",
            "arguments": "` + at + `",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.ValueEqual("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.ValueEqual("arguments", at)
			attrs.ValueEqual("worker", "print")
		})

		t.Run("AddFailure", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@at",
            "arguments": "garbage",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(400)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200)
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})

		t.Run("GetNotFound", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(404)
		})
	})

	t.Run("AddGetAndDeleteTriggerIn", func(t *testing.T) {
		var triggerID string

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@in",
            "arguments": "1s",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.ValueEqual("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.ValueEqual("type", "@in")
			attrs.ValueEqual("arguments", "1s")
			attrs.ValueEqual("worker", "print")
		})

		t.Run("AddFailure", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@in",
            "arguments": "garbage",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(400)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200)
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})

		t.Run("GetNotFound", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(404)
		})
	})

	t.Run("AddGetUpdateAndDeleteTriggerCron", func(t *testing.T) {
		var triggerID string

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@cron",
            "arguments": "0 0 0 * * 0",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.ValueEqual("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.ValueEqual("type", "@cron")
			attrs.ValueEqual("arguments", "0 0 0 * * 0")
			attrs.ValueEqual("worker", "print")
		})

		t.Run("PatchSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.PATCH("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "arguments": "0 0 0 * * 1"
          }
        }
      }`)).
				Expect().Status(200)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.ValueEqual("type", consts.Triggers)

			attrs := data.Value("attributes").Object()
			attrs.ValueEqual("type", "@cron")
			attrs.ValueEqual("arguments", "0 0 0 * * 1")
			attrs.ValueEqual("worker", "print")
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})
	})

	t.Run("AddTriggerWithMetadata", func(t *testing.T) {
		var triggerID string

		at := time.Now().Add(1100 * time.Millisecond).Format(time.RFC3339)

		t.Run("AddSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@webhook",
            "arguments": "` + at + `",
            "worker": "print",
            "message": "foo"
          }
        }
      }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			data := obj.Value("data").Object()
			triggerID = data.Value("id").String().NotEmpty().Raw()
			data.ValueEqual("type", consts.Triggers)
			data.Path("$.links.webhook").Equal("https://" + testInstance.Domain + "/jobs/webhooks/" + triggerID)

			attrs := data.Value("attributes").Object()
			attrs.ValueEqual("type", "@webhook")
			attrs.ValueEqual("arguments", at)
			attrs.ValueEqual("worker", "print")

			metas := attrs.Value("cozyMetadata").Object()
			metas.ValueEqual("doctypeVersion", "1")
			metas.ValueEqual("metadataVersion", 1)
			metas.ValueEqual("createdByApp", "CLI")
			metas.Value("createdAt").String().DateTime(time.RFC3339)
			metas.Value("updatedAt").String().DateTime(time.RFC3339)
		})

		t.Run("GetSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(200)
		})

		t.Run("DeleteSuccess", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.DELETE("/jobs/triggers/"+triggerID).
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(204)
		})
	})

	t.Run("GetAllJobs", func(t *testing.T) {
		tokenTriggers, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", consts.Triggers, "", time.Now())

		t.Run("GetNoJobs", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().
				Value("data").Array().Empty()
		})

		t.Run("CreateAJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				WithHeader("Content-Type", "application/json").
				// worker_arguments is deprecated but should still works
				// we are using it here to check that it still works
				WithBytes([]byte(`{
        "data": {
          "attributes": { 
            "type": "@in",
            "arguments": "10s",
            "worker": "print",
            "worker_arguments": "foo"
          }
        }
      }`)).
				Expect().Status(201)
		})

		t.Run("GetAllJobs", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Value("data").Array().Length().Equal(1)
			elem := obj.Value("data").Array().First().Object()
			elem.ValueEqual("type", consts.Triggers)
			attrs := elem.Value("attributes").Object()
			attrs.ValueEqual("type", "@in")
			attrs.ValueEqual("arguments", "10s")
			attrs.ValueEqual("worker", "print")
		})

		t.Run("WithWorkerQueryAndResult", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers").
				WithQuery("Worker", "print").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Value("data").Array().Length().Equal(1)
			elem := obj.Value("data").Array().First().Object()
			elem.ValueEqual("type", consts.Triggers)
			attrs := elem.Value("attributes").Object()
			attrs.ValueEqual("type", "@in")
			attrs.ValueEqual("arguments", "10s")
			attrs.ValueEqual("worker", "print")
		})

		t.Run("WithWorkerQueryAndNoResults", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			e.GET("/jobs/triggers").
				WithQuery("Worker", "nojobforme"). // no matching job
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object().Value("data").
				Array().Empty()
		})

		t.Run("WithTypeQuery", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.GET("/jobs/triggers").
				WithQuery("Type", "@in").
				WithHeader("Authorization", "Bearer "+tokenTriggers).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Value("data").Array().Length().Equal(1)
			elem := obj.Value("data").Array().First().Object()
			elem.ValueEqual("type", consts.Triggers)
			attrs := elem.Value("attributes").Object()
			attrs.ValueEqual("type", "@in")
			attrs.ValueEqual("arguments", "10s")
			attrs.ValueEqual("worker", "print")
		})
	})

	t.Run("ClientJobs", func(t *testing.T) {
		var triggerID string
		var jobID string

		scope := consts.Jobs + " " + consts.Triggers
		token, _ := testInstance.MakeJWT(consts.CLIAudience, "CLI", scope, "", time.Now())

		t.Run("CreateAClientJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers").
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
	       "data": {
	         "attributes": {
	           "type": "@client",
	           "message": "foobar"
	         }
	       }
	     }`)).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			triggerID = obj.Path("$.data.id").String().NotEmpty().Raw()

			attrs := obj.Path("$.data.attributes").Object()
			attrs.ValueEqual("type", "@client")
			attrs.ValueEqual("worker", "client")
		})

		t.Run("LaunchAClientJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.POST("/jobs/triggers/"+triggerID+"/launch").
				WithHeader("Authorization", "Bearer "+token).
				Expect().Status(201).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			jobID = obj.Path("$.data.id").String().NotEmpty().Raw()

			obj.Path("$.data.type").Equal(consts.Jobs)
			attrs := obj.Path("$.data.attributes").Object()
			attrs.ValueEqual("worker", "client")
			attrs.ValueEqual("state", job.Running)
			attrs.Value("queued_at").String().DateTime(time.RFC3339)
			attrs.Value("started_at").String().DateTime(time.RFC3339)
		})

		t.Run("PatchAClientJob", func(t *testing.T) {
			e := testutils.CreateTestClient(t, ts.URL)

			obj := e.PATCH("/jobs/"+jobID).
				WithHeader("Authorization", "Bearer "+token).
				WithHeader("Content-Type", "application/json").
				WithBytes([]byte(`{
	       "data": {
	         "attributes": {
	           "state": "errored",
	           "error": "LOGIN_FAILED"
	         }
	       }
	     }`)).
				Expect().Status(200).
				JSON(httpexpect.ContentOpts{MediaType: "application/vnd.api+json"}).
				Object()

			obj.Path("$.data.type").Equal(consts.Jobs)
			attrs := obj.Path("$.data.attributes").Object()
			attrs.ValueEqual("worker", "client")
			attrs.ValueEqual("state", job.Errored)
			attrs.ValueEqual("error", "LOGIN_FAILED")
			attrs.Value("queued_at").String().DateTime(time.RFC3339)
			attrs.Value("started_at").String().DateTime(time.RFC3339)
			attrs.Value("finished_at").String().DateTime(time.RFC3339)
		})
	})
}
