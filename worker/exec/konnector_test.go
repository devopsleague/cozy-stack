package exec

import (
	"errors"
	"fmt"
	"os"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/cozy/cozy-stack/model/account"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/i18n"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	"github.com/cozy/cozy-stack/tests/testutils"
	jwt "github.com/golang-jwt/jwt/v4"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecKonnector(t *testing.T) {
	if testing.Short() {
		t.Skip("a couchdb is required for this test: test skipped due to the use of --short flag")
	}

	config.UseTestFile(t)
	require.NoError(t, loadLocale(), "Could not load default locale translations")

	setup := testutils.NewSetup(t, t.Name())

	inst := setup.GetTestInstance()
	fs := inst.VFS()

	t.Run("with unknown domain", func(t *testing.T) {
		msg, err := job.NewMessage(map[string]interface{}{
			"konnector": "unknownapp",
		})
		assert.NoError(t, err)
		db := prefixer.NewPrefixer(0, "instance.does.not.exist", "instance.does.not.exist")
		j := job.NewJob(db, &job.JobRequest{
			Message:    msg,
			WorkerType: "konnector",
		})
		ctx := job.NewWorkerContext("id", j, nil).
			WithCookie(&konnectorWorker{})
		err = worker(ctx)
		assert.Error(t, err)
		assert.Equal(t, "Instance not found", err.Error())
	})

	t.Run("with unknown app", func(t *testing.T) {
		msg, err := job.NewMessage(map[string]interface{}{
			"konnector": "unknownapp",
		})
		assert.NoError(t, err)
		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "konnector",
		})
		ctx := job.NewWorkerContext("id", j, inst).
			WithCookie(&konnectorWorker{})
		err = worker(ctx)
		assert.Error(t, err)
		assert.Equal(t, "Application is not installed", err.Error())
	})

	t.Run("with a bad file exec", func(t *testing.T) {
		folderToSave := "7890"

		installer, err := app.NewInstaller(inst, app.Copier(consts.KonnectorType, inst),
			&app.InstallerOptions{
				Operation: app.Install,
				Type:      consts.KonnectorType,
				Slug:      "my-konnector-1",
				SourceURL: "git://github.com/konnectors/cozy-konnector-trainline.git",
			},
		)
		require.NoError(t, err)

		_, err = installer.RunSync()
		require.NoError(t, err)

		msg, err := job.NewMessage(map[string]interface{}{
			"konnector":      "my-konnector-1",
			"folder_to_save": folderToSave,
		})
		assert.NoError(t, err)

		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "konnector",
		})

		config.GetConfig().Konnectors.Cmd = ""
		ctx := job.NewWorkerContext("id", j, inst).
			WithCookie(&konnectorWorker{})
		err = worker(ctx)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "exec")

		config.GetConfig().Konnectors.Cmd = "echo"
		err = worker(ctx)
		assert.NoError(t, err)
	})

	t.Run("success", func(t *testing.T) {
		script := `#!/bin/bash

echo "{\"type\": \"toto\", \"message\": \"COZY_URL=${COZY_URL} ${COZY_CREDENTIALS}\"}"
echo "bad json"
echo "{\"type\": \"manifest\", \"message\": \"$(ls ${1}/manifest.konnector)\" }"
>&2 echo "log error"
`
		osFs := afero.NewOsFs()
		tmpScript := fmt.Sprintf("/tmp/test-konn-%d.sh", os.Getpid())
		defer func() { _ = osFs.RemoveAll(tmpScript) }()

		err := afero.WriteFile(osFs, tmpScript, []byte(script), 0)
		require.NoError(t, err)

		err = osFs.Chmod(tmpScript, 0777)
		require.NoError(t, err)

		installer, err := app.NewInstaller(inst, app.Copier(consts.KonnectorType, inst),
			&app.InstallerOptions{
				Operation: app.Install,
				Type:      consts.KonnectorType,
				Slug:      "my-konnector-1",
				SourceURL: "git://github.com/konnectors/cozy-konnector-trainline.git",
			},
		)
		if !errors.Is(err, app.ErrAlreadyExists) {
			require.NoError(t, err)

			_, err = installer.RunSync()
			require.NoError(t, err)
		}

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			evCh := realtime.GetHub().Subscriber(inst)
			evCh.Subscribe(consts.JobEvents)
			wg.Done()
			ch := evCh.Channel
			ev1 := <-ch
			ev2 := <-ch
			evCh.Close()
			doc1 := ev1.Doc.(*couchdb.JSONDoc)
			doc2 := ev2.Doc.(*couchdb.JSONDoc)

			assert.Equal(t, inst.Domain, ev1.Domain)
			assert.Equal(t, inst.Domain, ev2.Domain)

			assert.Equal(t, "toto", doc1.M["type"])
			assert.Equal(t, "manifest", doc2.M["type"])

			msg2 := doc2.M["message"].(string)
			assert.True(t, strings.HasPrefix(msg2, "/tmp"))
			assert.True(t, strings.HasSuffix(msg2, "/manifest.konnector"))

			msg1 := doc1.M["message"].(string)
			cozyURL := "COZY_URL=" + inst.PageURL("/", nil) + " "
			assert.True(t, strings.HasPrefix(msg1, cozyURL))
			token := msg1[len(cozyURL):]
			var claims permission.Claims
			err2 := crypto.ParseJWT(token, func(t *jwt.Token) (interface{}, error) {
				return inst.PickKey(t.Claims.(*permission.Claims).Audience)
			}, &claims)
			assert.NoError(t, err2)
			assert.Equal(t, consts.KonnectorAudience, claims.Audience)
			wg.Done()
		}()

		wg.Wait()
		wg.Add(1)
		msg, err := job.NewMessage(map[string]interface{}{
			"konnector": "my-konnector-1",
		})
		assert.NoError(t, err)

		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "konnector",
		})

		config.GetConfig().Konnectors.Cmd = tmpScript
		ctx := job.NewWorkerContext("id", j, inst).
			WithCookie(&konnectorWorker{})
		err = worker(ctx)
		assert.NoError(t, err)

		wg.Wait()
	})

	t.Run("with secret from accountType", func(t *testing.T) {
		script := `#!/bin/bash

SECRET=$(echo "$COZY_PARAMETERS" | sed -e 's/.*secret"://' -e 's/[},].*//')
echo "{\"type\": \"params\", \"message\": ${SECRET} }"
`
		osFs := afero.NewOsFs()
		tmpScript := fmt.Sprintf("/tmp/test-konn-%d.sh", os.Getpid())
		defer func() { _ = osFs.RemoveAll(tmpScript) }()

		err := afero.WriteFile(osFs, tmpScript, []byte(script), 0)
		require.NoError(t, err)

		err = osFs.Chmod(tmpScript, 0777)
		require.NoError(t, err)

		at := &account.AccountType{
			GrantMode: account.SecretGrant,
			Slug:      "my-konnector-1",
			Secret:    "s3cr3t",
		}
		err = couchdb.CreateDoc(prefixer.SecretsPrefixer, at)
		assert.NoError(t, err)
		defer func() {
			// Clean the account types
			ats, _ := account.FindAccountTypesBySlug("my-konnector-1", "all-contexts")
			for _, at = range ats {
				_ = couchdb.DeleteDoc(prefixer.SecretsPrefixer, at)
			}
		}()

		installer, err := app.NewInstaller(inst, app.Copier(consts.KonnectorType, inst),
			&app.InstallerOptions{
				Operation: app.Install,
				Type:      consts.KonnectorType,
				Slug:      "my-konnector-1",
				SourceURL: "git://github.com/konnectors/cozy-konnector-trainline.git",
			},
		)
		if !errors.Is(err, app.ErrAlreadyExists) {
			require.NoError(t, err)

			_, err = installer.RunSync()
			require.NoError(t, err)
		}

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			evCh := realtime.GetHub().Subscriber(inst)
			evCh.Subscribe(consts.JobEvents)
			wg.Done()
			ch := evCh.Channel
			ev1 := <-ch
			evCh.Close()
			doc1 := ev1.Doc.(*couchdb.JSONDoc)

			assert.Equal(t, inst.Domain, ev1.Domain)
			assert.Equal(t, "params", doc1.M["type"])
			msg1 := doc1.M["message"]
			assert.Equal(t, "s3cr3t", msg1)
			wg.Done()
		}()

		wg.Wait()
		wg.Add(1)
		msg, err := job.NewMessage(map[string]interface{}{
			"konnector": "my-konnector-1",
		})
		assert.NoError(t, err)

		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "konnector",
		})

		config.GetConfig().Konnectors.Cmd = tmpScript
		ctx := job.NewWorkerContext("id", j, inst).
			WithCookie(&konnectorWorker{})
		err = worker(ctx)
		assert.NoError(t, err)

		wg.Wait()
	})

	t.Run("create folder", func(t *testing.T) {
		script := `#!/bin/bash

echo "{\"type\": \"toto\", \"message\": \"COZY_URL=${COZY_URL}\"}"
`
		osFs := afero.NewOsFs()
		tmpScript := fmt.Sprintf("/tmp/test-konn-%d.sh", os.Getpid())
		defer func() { _ = osFs.RemoveAll(tmpScript) }()

		err := afero.WriteFile(osFs, tmpScript, []byte(script), 0)
		require.NoError(t, err)

		err = osFs.Chmod(tmpScript, 0777)
		require.NoError(t, err)

		installer, err := app.NewInstaller(inst, app.Copier(consts.KonnectorType, inst),
			&app.InstallerOptions{
				Operation: app.Install,
				Type:      consts.KonnectorType,
				Slug:      "my-konnector-1",
				SourceURL: "git://github.com/konnectors/cozy-konnector-trainline.git",
			},
		)
		if !errors.Is(err, app.ErrAlreadyExists) {
			require.NoError(t, err)

			_, err = installer.RunSync()
			require.NoError(t, err)
		}

		var wg sync.WaitGroup
		wg.Add(1)

		go func() {
			evCh := realtime.GetHub().Subscriber(inst)
			evCh.Subscribe(consts.Files)
			wg.Done()
			ch := evCh.Channel

			// for DefaultFolderPath
			for ev := range ch {
				doc := ev.Doc.(*vfs.DirDoc)
				if doc.DocName == "toto" {
					assert.Equal(t, inst.Domain, ev.Domain)
					wg.Done()
					break
				}
			}

			// for Konnector name and Account name
			for ev := range ch {
				doc := ev.Doc.(*vfs.DirDoc)
				if doc.DocName == "account-1" {
					assert.Equal(t, inst.Domain, ev.Domain)
					wg.Done()
					break
				}
			}
		}()

		wg.Wait()

		acc := &account.Account{}

		// Folder is created from DefaultFolderPath
		wg.Add(1)
		acc.DefaultFolderPath = "/Administrative/toto"
		require.NoError(t, couchdb.CreateDoc(inst, acc))
		defer func() { _ = couchdb.DeleteDoc(inst, acc) }()

		msg, err := job.NewMessage(map[string]interface{}{
			"konnector":      "my-konnector-1",
			"folder_to_save": "id-of-a-deleted-folder",
			"account":        acc.ID(),
		})
		require.NoError(t, err)

		j := job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "konnector",
		})

		config.GetConfig().Konnectors.Cmd = tmpScript
		ctx := job.NewWorkerContext("id", j, inst).
			WithCookie(&konnectorWorker{})
		err = worker(ctx)
		require.NoError(t, err)

		wg.Wait()

		dir, err := fs.DirByPath("/Administrative/toto")
		assert.NoError(t, err)
		assert.Len(t, dir.ReferencedBy, 1)
		assert.Equal(t, dir.ReferencedBy[0].ID, "io.cozy.konnectors/my-konnector-1")
		assert.Equal(t, "my-konnector-1", dir.CozyMetadata.CreatedByApp)
		assert.Contains(t, dir.CozyMetadata.CreatedOn, inst.Domain)
		assert.Len(t, dir.CozyMetadata.UpdatedByApps, 1)
		assert.Equal(t, dir.CozyMetadata.SourceAccount, acc.ID())
		require.NoError(t, fs.DestroyDirAndContent(dir, fs.EnsureErased))

		// Folder is created from Konnector name and Account name
		wg.Add(1)
		acc.DefaultFolderPath = ""
		acc.Name = "account-1"
		require.NoError(t, couchdb.UpdateDoc(inst, acc))

		msg, err = job.NewMessage(map[string]interface{}{
			"konnector":      "my-konnector-1",
			"folder_to_save": "id-of-a-deleted-folder",
			"account":        acc.ID(),
		})
		require.NoError(t, err)

		j = job.NewJob(inst, &job.JobRequest{
			Message:    msg,
			WorkerType: "konnector",
		})

		origCmd := config.GetConfig().Konnectors.Cmd
		config.GetConfig().Konnectors.Cmd = tmpScript
		defer func() { config.GetConfig().Konnectors.Cmd = origCmd }()

		ctx = job.NewWorkerContext("id", j, inst).
			WithCookie(&konnectorWorker{})
		err = worker(ctx)
		require.NoError(t, err)

		wg.Wait()

		dir, err = fs.DirByPath("/Administrative/Trainline/account-1")
		require.NoError(t, err)
		require.Len(t, dir.ReferencedBy, 1)
		assert.Equal(t, dir.ReferencedBy[0].ID, "io.cozy.konnectors/my-konnector-1")
		assert.Equal(t, "my-konnector-1", dir.CozyMetadata.CreatedByApp)
		assert.Contains(t, dir.CozyMetadata.CreatedOn, inst.Domain)
		assert.Len(t, dir.CozyMetadata.UpdatedByApps, 1)
		assert.Equal(t, dir.CozyMetadata.SourceAccount, acc.ID())

		var updatedAcc account.Account
		err = couchdb.GetDoc(inst, consts.Accounts, acc.ID(), &updatedAcc)
		require.NoError(t, err)
		assert.Equal(t, updatedAcc.DefaultFolderPath, "/Administrative/Trainline/account-1")
	})
}

func loadLocale() error {
	locale := consts.DefaultLocale
	assetsPath := config.GetConfig().Assets
	if assetsPath != "" {
		pofile := path.Join("../..", assetsPath, "locales", locale+".po")
		po, err := os.ReadFile(pofile)
		if err != nil {
			return fmt.Errorf("Can't load the po file for %s", locale)
		}
		i18n.LoadLocale(locale, "", po)
	}
	return nil
}
