package exec

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/appfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/spf13/afero"
)

// ServiceOptions contains the options to execute a service.
type ServiceOptions struct {
	Slug   string          `json:"slug"`   // The application slug
	Name   string          `json:"name"`   // The service name
	Fields json.RawMessage `json:"fields"` // Custom fields
	File   string          `json:"service_file"`

	Message *ServiceOptions `json:"message"`
}

type serviceWorker struct {
	man     *app.WebappManifest
	slug    string
	name    string
	fields  json.RawMessage
	workDir string
}

func (w *serviceWorker) PrepareWorkDir(ctx *job.WorkerContext, i *instance.Instance) (workDir string, cleanDir func(), err error) {
	cleanDir = func() {}
	opts := &ServiceOptions{}
	if err = ctx.UnmarshalMessage(&opts); err != nil {
		return
	}
	if opts.Message != nil {
		opts = opts.Message
	}

	slug := opts.Slug
	name := opts.Name
	fields := opts.Fields

	man, err := app.GetWebappBySlugAndUpdate(i, slug,
		app.Copier(consts.WebappType, i), i.Registries())
	if err != nil {
		if errors.Is(err, app.ErrNotFound) {
			err = job.BadTriggerError{Err: err}
		}
		return
	}

	w.slug = slug
	w.name = name
	w.fields = fields

	// Upgrade "installed" to "ready"
	if err = app.UpgradeInstalledState(i, man); err != nil {
		return
	}

	if man.State() != app.Ready {
		err = errors.New("Application is not ready")
		return
	}

	var service *app.Service
	var ok bool
	services := man.Services()
	if name != "" {
		service, ok = services[name]
	} else {
		for _, s := range services {
			if s.File == opts.File {
				service, ok = s, true
				break
			}
		}
	}
	if !ok {
		err = job.BadTriggerError{Err: fmt.Errorf("Service %q was not found", name)}
		return
	}
	// Check if the trigger is orphan
	if triggerID, ok := ctx.TriggerID(); ok && service.TriggerID != "" {
		if triggerID != service.TriggerID {
			// Check if this is another trigger for the same declared service
			var tInfos job.TriggerInfos
			err = couchdb.GetDoc(i, consts.Triggers, triggerID, &tInfos)
			if err != nil {
				err = job.BadTriggerError{Err: fmt.Errorf("Trigger %q not found", triggerID)}
				return
			}
			var msg ServiceOptions
			err = json.Unmarshal(tInfos.Message, &msg)
			if err != nil {
				err = job.BadTriggerError{Err: fmt.Errorf("Trigger %q has bad message structure", triggerID)}
				return
			}
			if msg.Name != name {
				err = job.BadTriggerError{Err: fmt.Errorf("Trigger %q is orphan", triggerID)}
				return
			}
		}
	}

	w.man = man

	osFS := afero.NewOsFs()
	workDir, err = afero.TempDir(osFS, "", "service-"+slug)
	if err != nil {
		return
	}
	cleanDir = func() {
		_ = os.RemoveAll(workDir)
	}
	w.workDir = workDir
	workFS := afero.NewBasePathFs(osFS, workDir)

	var fs appfs.FileServer
	if man.FromAppsDir {
		fs = app.FSForAppDir(man.Slug())
	} else {
		fs = app.AppsFileServer(i)
	}
	src, err := fs.Open(man.Slug(), man.Version(), man.Checksum(), path.Join("/", service.File))
	if err != nil {
		return
	}
	defer src.Close()

	dst, err := workFS.OpenFile("index.js", os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return
	}
	defer dst.Close()

	_, err = io.Copy(dst, src)
	if err != nil {
		return
	}

	return workDir, cleanDir, nil
}

func (w *serviceWorker) Slug() string {
	return w.slug
}

func (w *serviceWorker) PrepareCmdEnv(ctx *job.WorkerContext, i *instance.Instance) (cmd string, env []string, err error) {
	type serviceEvent struct {
		Doc interface{} `json:"doc"`
	}

	var doc serviceEvent
	marshaled := []byte{}
	if err := ctx.UnmarshalEvent(&doc); err == nil {
		marshaled, err = json.Marshal(doc.Doc)
		if err != nil {
			return "", nil, err
		}
	}

	payload, err := preparePayload(ctx, w.workDir)
	if err != nil {
		return "", nil, err
	}

	token := i.BuildAppToken(w.man.Slug(), "")
	cmd = config.GetConfig().Konnectors.Cmd
	env = []string{
		"COZY_URL=" + i.PageURL("/", nil),
		"COZY_CREDENTIALS=" + token,
		"COZY_LANGUAGE=node", // default to node language for services
		"COZY_LOCALE=" + i.Locale,
		"COZY_TIME_LIMIT=" + ctxToTimeLimit(ctx),
		"COZY_JOB_ID=" + ctx.ID(),
		"COZY_COUCH_DOC=" + string(marshaled),
		"COZY_PAYLOAD=" + payload,
		"COZY_FIELDS=" + string(w.fields),
	}
	if triggerID, ok := ctx.TriggerID(); ok {
		env = append(env, "COZY_TRIGGER_ID="+triggerID)
	}
	return
}

func (w *serviceWorker) Logger(ctx *job.WorkerContext) logger.Logger {
	log := ctx.Logger().WithField("slug", w.Slug())
	if w.name != "" {
		log = log.WithField("name", w.name)
	}
	return log
}

func (w *serviceWorker) ScanOutput(ctx *job.WorkerContext, i *instance.Instance, line []byte) error {
	var msg struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(line, &msg); err != nil {
		return fmt.Errorf("Could not parse stdout as JSON: %q", string(line))
	}

	// Truncate very long messages
	if len(msg.Message) > 4000 {
		msg.Message = msg.Message[:4000]
	}

	log := w.Logger(ctx)
	switch msg.Type {
	case konnectorMsgTypeDebug, konnectorMsgTypeInfo:
		log.Debug(msg.Message)
	case konnectorMsgTypeWarning, "warn":
		log.Warn(msg.Message)
	case konnectorMsgTypeError:
		log.Error(msg.Message)
	case konnectorMsgTypeCritical:
		log.Error(msg.Message)
	}
	return nil
}

func (w *serviceWorker) Error(i *instance.Instance, err error) error {
	return err
}

func (w *serviceWorker) Commit(ctx *job.WorkerContext, errjob error) error {
	log := w.Logger(ctx)
	if errjob == nil {
		log.Info("Service success")
	} else {
		log.Infof("Service failure: %s", errjob)
	}
	return nil
}
