package sharing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/realtime"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

// UploadMsg is used for jobs on the share-upload worker.
type UploadMsg struct {
	SharingID string `json:"sharing_id"`
	Errors    int    `json:"errors"`
}

// Upload starts uploading files for this sharing
func (s *Sharing) Upload(inst *instance.Instance, errors int) error {
	mu := config.Lock().ReadWrite(inst, "sharings/"+s.SID+"/upload")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	var errm error
	var members []*Member
	if !s.Owner {
		members = append(members, &s.Members[0])
	} else {
		for i, m := range s.Members {
			if i == 0 {
				continue
			}
			if m.Status == MemberStatusReady {
				members = append(members, &s.Members[i])
			}
		}
	}

	lastTry := errors+1 == MaxRetries
	for i := 0; i < BatchSize; i++ {
		if len(members) == 0 {
			break
		}
		m := members[0]
		members = members[1:]
		more, err := s.UploadTo(inst, m, lastTry)
		if err != nil {
			errm = multierror.Append(errm, err)
		}
		if more {
			members = append(members, m)
		}
	}

	if errm != nil {
		s.retryWorker(inst, "share-upload", errors)
		inst.Logger().WithNamespace("upload").Infof("errm=%s\n", errm)
	} else if len(members) > 0 {
		s.pushJob(inst, "share-upload")
	}
	return errm
}

// InitialUpload uploads files to just a member, for the first time
func (s *Sharing) InitialUpload(inst *instance.Instance, m *Member) error {
	mu := config.Lock().ReadWrite(inst, "sharings/"+s.SID+"/upload")
	if err := mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	for i := 0; i < BatchSize; i++ {
		more, err := s.UploadTo(inst, m, false)
		if err != nil {
			return err
		}
		if !more {
			return s.sendInitialEndNotif(inst, m)
		}
	}

	s.pushJob(inst, "share-upload")
	return nil
}

// sendInitialEndNotif sends a notification to the recipient that the initial
// sync is finished
func (s *Sharing) sendInitialEndNotif(inst *instance.Instance, m *Member) error {
	u, err := url.Parse(m.Instance)
	if err != nil {
		return err
	}
	c := s.FindCredentials(m)
	if c == nil || c.AccessToken == nil {
		return ErrInvalidSharing
	}
	opts := &request.Options{
		Method: http.MethodDelete,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   fmt.Sprintf("/sharings/%s/initial", s.SID),
		Headers: request.Headers{
			echo.HeaderAuthorization: "Bearer " + c.AccessToken.AccessToken,
		},
	}
	res, err := request.Req(opts)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

// UploadTo uploads one file to the given member. It returns false if there
// are no more files to upload to this member currently.
func (s *Sharing) UploadTo(inst *instance.Instance, m *Member, lastTry bool) (bool, error) {
	if m.Instance == "" {
		return false, ErrInvalidURL
	}
	creds := s.FindCredentials(m)
	if creds == nil {
		return false, ErrInvalidSharing
	}

	lastSeq, err := s.getLastSeqNumber(inst, m, "upload")
	if err != nil {
		return false, err
	}
	inst.Logger().WithNamespace("upload").Debugf("lastSeq = %s", lastSeq)

	file, ruleIndex, seq, err := s.findNextFileToUpload(inst, lastSeq)
	if errors.Is(err, ErrInternalServerError) {
		// Retrying is useless in this case, let's skip this file
		if seq != lastSeq {
			_ = s.UpdateLastSequenceNumber(inst, m, "upload", seq)
		}
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if file == nil {
		if seq != lastSeq {
			err = s.UpdateLastSequenceNumber(inst, m, "upload", seq)
		}
		return false, err
	}

	if err = s.uploadFile(inst, m, file, ruleIndex); err != nil {
		if lastTry {
			_ = s.UpdateLastSequenceNumber(inst, m, "upload", seq)
		}
		return false, err
	}

	return true, s.UpdateLastSequenceNumber(inst, m, "upload", seq)
}

// findNextFileToUpload uses the changes feed to find the next file that needs
// to be uploaded. It returns a file document if there is one file to upload,
// and the sequence number where it is in the changes feed.
func (s *Sharing) findNextFileToUpload(inst *instance.Instance, since string) (map[string]interface{}, int, string, error) {
	for {
		response, err := couchdb.GetChanges(inst, &couchdb.ChangesRequest{
			DocType:     consts.Shared,
			IncludeDocs: true,
			Since:       since,
			Limit:       1,
		})
		if err != nil {
			return nil, 0, since, err
		}
		since = response.LastSeq
		if len(response.Results) == 0 {
			break
		}
		r := response.Results[0]
		infos, ok := r.Doc.Get("infos").(map[string]interface{})
		if !ok {
			continue
		}
		info, ok := infos[s.SID].(map[string]interface{})
		if !ok {
			continue
		}
		if _, ok = info["binary"]; !ok {
			continue
		}
		if _, ok = info["removed"]; ok {
			continue
		}
		idx, ok := info["rule"].(float64)
		if !ok {
			continue
		}
		rev := extractLastRevision(r.Doc)
		if rev == "" {
			continue
		}
		docID := strings.SplitN(r.DocID, "/", 2)[1]
		ir := couchdb.IDRev{ID: docID, Rev: rev}
		query := []couchdb.IDRev{ir}
		results, err := couchdb.BulkGetDocs(inst, consts.Files, query)
		if err != nil {
			return nil, 0, since, err
		}
		if len(results) == 0 {
			inst.Logger().WithNamespace("upload").
				Warnf("missing results for bulk get %v", query)
			return nil, 0, since, ErrInternalServerError
		}
		if results[0]["_deleted"] == true {
			inst.Logger().WithNamespace("upload").
				Warnf("cannot upload _deleted file %v", results[0])
			return nil, 0, since, ErrInternalServerError
		}
		return results[0], int(idx), since, nil
	}
	return nil, 0, since, nil
}

// uploadFile uploads one file to the given member. It first try to just send
// the metadata, and if it is not enough, it also send the binary.
func (s *Sharing) uploadFile(inst *instance.Instance, m *Member, file map[string]interface{}, ruleIndex int) error {
	inst.Logger().WithNamespace("upload").Debugf("going to upload %#v", file)

	// Do not try to send a trashed file, the trash status will be synchronized
	// via the CouchDB replication protocol
	if trashed, _ := file["trashed"].(bool); trashed {
		return nil
	}

	creds := s.FindCredentials(m)
	if creds == nil {
		return ErrInvalidSharing
	}
	u, err := url.Parse(m.Instance)
	if err != nil {
		return err
	}
	origFileID := file["_id"].(string)
	s.TransformFileToSent(file, creds.XorKey, ruleIndex)
	xoredFileID := file["_id"].(string)
	body, err := json.Marshal(file)
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method:  http.MethodPut,
		Scheme:  u.Scheme,
		Domain:  u.Host,
		Path:    "/sharings/" + s.SID + "/io.cozy.files/" + xoredFileID + "/metadata",
		Queries: url.Values{"from": {inst.ContextualDomain()}},
		Headers: request.Headers{
			echo.HeaderAccept:        echo.MIMEApplicationJSON,
			echo.HeaderContentType:   echo.MIMEApplicationJSON,
			echo.HeaderAuthorization: "Bearer " + creds.AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	var res *http.Response
	res, err = request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, err, s, m, creds, opts, body)
	}
	if err != nil {
		if res != nil && res.StatusCode/100 == 5 {
			inst.Logger().WithNamespace("upload").
				Warnf("%s got response %d", opts.Path, res.StatusCode)
			return ErrInternalServerError
		}
		return err
	}
	defer res.Body.Close()

	if res.StatusCode == 204 {
		return nil
	}
	var resBody KeyToUpload
	if err = json.NewDecoder(res.Body).Decode(&resBody); err != nil {
		return err
	}

	fs := inst.VFS()
	fileDoc, err := fs.FileByID(origFileID)
	if err != nil {
		return err
	}
	content, err := fs.OpenFile(fileDoc)
	if err != nil {
		return err
	}
	defer content.Close()

	opts2 := &request.Options{
		Method:  http.MethodPut,
		Scheme:  u.Scheme,
		Domain:  u.Host,
		Path:    "/sharings/" + s.SID + "/io.cozy.files/" + resBody.Key,
		Queries: url.Values{"from": {inst.ContextualDomain()}},
		Headers: request.Headers{
			echo.HeaderContentType:   fileDoc.Mime,
			echo.HeaderAuthorization: "Bearer " + creds.AccessToken.AccessToken,
		},
		Body:   content,
		Client: http.DefaultClient,
	}
	res2, err := request.Req(opts2)
	if err != nil {
		if res2 != nil && res2.StatusCode/100 == 5 {
			inst.Logger().WithNamespace("upload").
				Warnf("%s got response %d", opts2.Path, res2.StatusCode)
			return ErrInternalServerError
		}
		return err
	}
	res2.Body.Close()
	return nil
}

// FileDocWithRevisions is the struct of the payload for synchronizing a file
type FileDocWithRevisions struct {
	*vfs.FileDoc
	Revisions RevsStruct `json:"_revisions"`
}

// Clone is part of the couchdb.Doc interface
func (f *FileDocWithRevisions) Clone() couchdb.Doc {
	panic("FileDocWithRevisions must not be cloned")
}

// KeyToUpload contains the key for uploading a file (when syncing metadata is
// not enough)
type KeyToUpload struct {
	Key string `json:"key"`
}

func (s *Sharing) createUploadKey(inst *instance.Instance, target *FileDocWithRevisions) (*KeyToUpload, error) {
	key, err := getStore().Save(inst, target)
	if err != nil {
		return nil, err
	}
	return &KeyToUpload{Key: key}, nil
}

// SyncFile tries to synchronize a file with just the metadata. If it can't,
// it will return a key to upload the content.
func (s *Sharing) SyncFile(inst *instance.Instance, target *FileDocWithRevisions) (*KeyToUpload, error) {
	inst.Logger().WithNamespace("upload").Debugf("SyncFile %#v", target)

	if len(target.MD5Sum) == 0 {
		return nil, vfs.ErrInvalidHash
	}
	sid := consts.Files + "/" + target.DocID
	mu := config.Lock().ReadWrite(inst, "shared/"+sid)
	if err := mu.Lock(); err != nil {
		return nil, err
	}
	defer mu.Unlock()
	current, err := inst.VFS().FileByID(target.DocID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if rule, _ := s.findRuleForNewFile(target.FileDoc); rule == nil {
				return nil, ErrSafety
			}
			return s.createUploadKey(inst, target)
		}
		return nil, err
	}
	var ref SharedRef
	err = couchdb.GetDoc(inst, consts.Shared, sid, &ref)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil, ErrSafety
		}
		return nil, err
	}
	if infos, ok := ref.Infos[s.SID]; !ok || (infos.Removed && !infos.Dissociated) {
		return nil, ErrSafety
	}
	if sub, _ := ref.Revisions.Find(target.DocRev); sub != nil {
		// It's just the echo, there is nothing to do
		return nil, nil
	}
	if !bytes.Equal(target.MD5Sum, current.MD5Sum) {
		return s.createUploadKey(inst, target)
	}
	return nil, s.updateFileMetadata(inst, target, current, &ref)
}

// prepareFileWithAncestors find the parent directory for file, and recreates it
// if it is missing.
func (s *Sharing) prepareFileWithAncestors(inst *instance.Instance, newdoc *vfs.FileDoc, dirID string) error {
	// Case 1: there is a rule for sharing this file
	if s.hasExplicitRuleForFile(newdoc) {
		return nil
	}

	// Case 2: the file is in a directory that is shared
	if dirID == "" {
		parent, err := s.GetSharingDir(inst)
		if err != nil {
			return err
		}
		newdoc.DirID = parent.DocID
	} else if dirID != newdoc.DirID {
		parent, err := inst.VFS().DirByID(dirID)
		if errors.Is(err, os.ErrNotExist) {
			parent, err = s.recreateParent(inst, dirID)
		}
		if err != nil {
			inst.Logger().WithNamespace("upload").
				Debugf("Conflict for parent on sync file: %s", err)
			return err
		}
		newdoc.DirID = parent.DocID
	}
	return nil
}

// updateFileMetadata updates a file document when only some metadata has
// changed, but not the content.
func (s *Sharing) updateFileMetadata(inst *instance.Instance, target *FileDocWithRevisions, newdoc *vfs.FileDoc, ref *SharedRef) error {
	indexer := newSharingIndexer(inst, &bulkRevs{
		Rev:       target.DocRev,
		Revisions: target.Revisions,
	}, ref)

	chain := revsStructToChain(target.Revisions)
	conflict := detectConflict(newdoc.DocRev, chain)
	switch conflict {
	case LostConflict:
		return nil
	case WonConflict:
		indexer.WillResolveConflict(newdoc.DocRev, chain)
	case NoConflict:
		// Nothing to do
	}

	fs := inst.VFS().UseSharingIndexer(indexer)
	olddoc := newdoc.Clone().(*vfs.FileDoc)
	newdoc.DocName = target.DocName
	if err := s.prepareFileWithAncestors(inst, newdoc, target.DirID); err != nil {
		return err
	}
	newdoc.ResetFullpath()
	copySafeFieldsToFile(target.FileDoc, newdoc)
	infos := ref.Infos[s.SID]
	rule := &s.Rules[infos.Rule]
	newdoc.ReferencedBy = buildReferencedBy(target.FileDoc, newdoc, rule)

	err := fs.UpdateFileDoc(olddoc, newdoc)
	if errors.Is(err, os.ErrExist) {
		pth, errp := newdoc.Path(fs)
		if errp != nil {
			return errp
		}
		name, errr := s.resolveConflictSamePath(inst, newdoc.DocID, pth)
		if errr != nil {
			return errr
		}
		if name != "" {
			indexer.IncrementRevision()
			newdoc.DocName = name
			newdoc.ResetFullpath()
		}
		err = fs.UpdateFileDoc(olddoc, newdoc)
	}
	if err != nil {
		inst.Logger().WithNamespace("upload").
			Debugf("Cannot update file: %s", err)
		return err
	}
	return nil
}

// HandleFileUpload is used to receive a file upload when synchronizing just
// the metadata was not enough.
func (s *Sharing) HandleFileUpload(inst *instance.Instance, key string, body io.ReadCloser) error {
	defer body.Close()
	target, err := getStore().Get(inst, key)
	if err != nil {
		return err
	}
	if target == nil {
		return ErrMissingFileMetadata
	}
	inst.Logger().WithNamespace("upload").Debugf("HandleFileUpload %#v %#v", target.FileDoc, target.Revisions)
	sid := consts.Files + "/" + target.DocID
	mu := config.Lock().ReadWrite(inst, "shared/"+sid)
	if err = mu.Lock(); err != nil {
		return err
	}
	defer mu.Unlock()

	current, err := inst.VFS().FileByID(target.DocID)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		inst.Logger().WithNamespace("upload").
			Warnf("Upload has failed: %s", err)
		return err
	}

	if current == nil {
		return s.UploadNewFile(inst, target, body)
	}
	return s.UploadExistingFile(inst, target, current, body)
}

// UploadNewFile is used to receive a new file.
func (s *Sharing) UploadNewFile(inst *instance.Instance, target *FileDocWithRevisions, body io.ReadCloser) error {
	inst.Logger().WithNamespace("upload").Debugf("UploadNewFile")
	ref := SharedRef{
		Infos: make(map[string]SharedInfo),
	}
	indexer := newSharingIndexer(inst, &bulkRevs{
		Rev:       target.Rev(),
		Revisions: target.Revisions,
	}, &ref)
	fs := inst.VFS().UseSharingIndexer(indexer)

	rule, ruleIndex := s.findRuleForNewFile(target.FileDoc)
	if rule == nil {
		return ErrSafety
	}

	var err error
	var parent *vfs.DirDoc
	var addReferencedBy bool
	if target.DirID != "" {
		parent, err = fs.DirByID(target.DirID)
		if errors.Is(err, os.ErrNotExist) {
			parent, err = s.recreateParent(inst, target.DirID)
		}
		if err != nil {
			inst.Logger().WithNamespace("upload").
				Infof("Conflict for parent on file upload: %s", err)
		}
	} else if target.DocID == rule.Values[0] {
		parentID := s.cleanShortcutID(inst)
		if parentID != "" {
			parent, err = fs.DirByID(parentID)
		} else {
			parent, err = EnsureSharedWithMeDir(inst)
		}
		addReferencedBy = true
	} else {
		parent, err = s.GetSharingDir(inst)
	}
	if err != nil {
		return err
	}

	// XXX In some cases, we have to add a fake revision to the created file
	// because it was already known by CouchDB. For example:
	// - Alice has a shared folder with Bob
	// - inside this folder, there is a directory named foo
	// - there is a file named bar inside foo, at the revision 1-123
	// - Alice moves foo outside of the sharing
	// - the sharing replication deletes bar on Bob's instance (revision 2-456)
	// - later, Alice moves again foo inside the sharing (with bar)
	// - we are in this function for bar, and if we try to recreate the file
	//   with revision 1-123, it will still be seen as deleted by CouchDB
	//   (revision 2-456 wins) => so, we create a revision 2-789.
	var fake couchdb.JSONDoc
	if err := couchdb.GetDoc(inst, consts.Files, target.DocID, &fake); couchdb.IsDeletedError(err) {
		indexer.IncrementRevision()
	}

	newdoc, err := vfs.NewFileDoc(target.DocName, parent.DocID, target.Size(), target.MD5Sum,
		target.Mime, target.Class, target.CreatedAt, target.Executable, false, false, target.Tags)
	if err != nil {
		return err
	}
	newdoc.SetID(target.DocID)
	ref.SID = consts.Files + "/" + newdoc.DocID
	copySafeFieldsToFile(target.FileDoc, newdoc)

	ref.Infos[s.SID] = SharedInfo{Rule: ruleIndex, Binary: true}
	newdoc.ReferencedBy = buildReferencedBy(target.FileDoc, nil, rule)
	if addReferencedBy {
		ref := couchdb.DocReference{
			ID:   s.SID,
			Type: consts.Sharings,
		}
		newdoc.ReferencedBy = append(newdoc.ReferencedBy, ref)
	}

	file, err := fs.CreateFile(newdoc, nil)
	if errors.Is(err, os.ErrExist) {
		pth, errp := newdoc.Path(fs)
		if errp != nil {
			return errp
		}
		name, errr := s.resolveConflictSamePath(inst, newdoc.DocID, pth)
		if errr != nil {
			return errr
		}
		if name != "" {
			indexer.IncrementRevision()
			newdoc.DocName = name
			newdoc.ResetFullpath()
		}
		file, err = fs.CreateFile(newdoc, nil)
	}
	if err != nil {
		inst.Logger().WithNamespace("upload").
			Debugf("Cannot create file: %s", err)
		return err
	}
	if s.NbFiles > 0 {
		defer s.countReceivedFiles(inst)
	}
	return copyFileContent(inst, file, body)
}

// countReceivedFiles counts the number of files received during the initial
// sync, and pushs an event to the real-time system with this count
func (s *Sharing) countReceivedFiles(inst *instance.Instance) {
	count := 0
	req := &couchdb.ViewRequest{
		Key:         s.SID,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.SharedDocsBySharingID, req, &res)
	if err == nil {
		for _, row := range res.Rows {
			var doc SharedRef
			if err = json.Unmarshal(row.Doc, &doc); err != nil {
				continue
			}
			if doc.Infos[s.SID].Binary {
				count++
			}
		}
	}

	if count >= s.NbFiles {
		if err = s.EndInitial(inst); err != nil {
			inst.Logger().WithNamespace("sharing").
				Errorf("Can't save sharing %v: %s", s, err)
		}
		return
	}

	doc := couchdb.JSONDoc{
		Type: consts.SharingsInitialSync,
		M: map[string]interface{}{
			"_id":   s.SID,
			"count": count,
		},
	}
	realtime.GetHub().Publish(inst, realtime.EventUpdate, &doc, nil)
}

// UploadExistingFile is used to receive new content for an existing file.
//
// Note: if file was renamed + its content has changed, we modify the content
// first, then rename it, not trying to do both at the same time. We do it in
// this order because the difficult case is if one operation succeeds and the
// other fails (if the two succeeds, we are fine; if the two fails, we just
// retry), and in that case, it is easier to manage a conflict on dir_id+name
// than on content: a conflict on different content is resolved by a copy of
// the file (which is not what we want), a conflict of name+dir_id, the higher
// revision wins and it should be the good one in our case.
func (s *Sharing) UploadExistingFile(inst *instance.Instance, target *FileDocWithRevisions, newdoc *vfs.FileDoc, body io.ReadCloser) error {
	inst.Logger().WithNamespace("upload").Debugf("UploadExistingFile")
	var ref SharedRef
	err := couchdb.GetDoc(inst, consts.Shared, consts.Files+"/"+target.DocID, &ref)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return ErrSafety
		}
		return err
	}
	indexer := newSharingIndexer(inst, &bulkRevs{
		Rev:       target.Rev(),
		Revisions: target.Revisions,
	}, &ref)
	fs := inst.VFS().UseSharingIndexer(indexer)
	olddoc := newdoc.Clone().(*vfs.FileDoc)

	infos, ok := ref.Infos[s.SID]
	if !ok || (infos.Removed && !infos.Dissociated) {
		return ErrSafety
	}
	rule := &s.Rules[infos.Rule]
	newdoc.ReferencedBy = buildReferencedBy(target.FileDoc, olddoc, rule)
	copySafeFieldsToFile(target.FileDoc, newdoc)
	newdoc.DocName = target.DocName
	if err := s.prepareFileWithAncestors(inst, newdoc, target.DirID); err != nil {
		return err
	}
	newdoc.ResetFullpath()
	newdoc.ByteSize = target.ByteSize
	newdoc.MD5Sum = target.MD5Sum

	chain := revsStructToChain(target.Revisions)
	conflict := detectConflict(newdoc.DocRev, chain)
	switch conflict {
	case LostConflict:
		return s.uploadLostConflict(inst, target, newdoc, body)
	case WonConflict:
		if err = s.uploadWonConflict(inst, olddoc); err != nil {
			return err
		}
	case NoConflict:
		// Nothing to do
	}
	indexer.WillResolveConflict(newdoc.DocRev, chain)

	// Easy case: only the content has changed, not its path
	if newdoc.DocName == olddoc.DocName && newdoc.DirID == olddoc.DirID {
		file, errf := fs.CreateFile(newdoc, olddoc)
		if errf != nil {
			return errf
		}
		return copyFileContent(inst, file, body)
	}

	stash := indexer.StashRevision(false)
	tmpdoc := newdoc.Clone().(*vfs.FileDoc)
	tmpdoc.DocName = olddoc.DocName
	tmpdoc.DirID = olddoc.DirID
	tmpdoc.ResetFullpath()
	file, err := fs.CreateFile(tmpdoc, olddoc)
	if err != nil {
		return err
	}
	if err = copyFileContent(inst, file, body); err != nil {
		return err
	}

	indexer.UnstashRevision(stash)
	newdoc.DocRev = tmpdoc.DocRev
	newdoc.InternalID = tmpdoc.InternalID
	err = fs.UpdateFileDoc(tmpdoc, newdoc)
	if errors.Is(err, os.ErrExist) {
		pth, errp := newdoc.Path(fs)
		if errp != nil {
			return errp
		}
		name, errr := s.resolveConflictSamePath(inst, newdoc.DocID, pth)
		if errr != nil {
			return errr
		}
		if name != "" {
			indexer.IncrementRevision()
			newdoc.DocName = name
			newdoc.ResetFullpath()
		}
		err = fs.UpdateFileDoc(tmpdoc, newdoc)
	}
	return err
}

// uploadLostConflict manages an upload where a file is in conflict, and the
// uploaded file version goes to a new file.
func (s *Sharing) uploadLostConflict(inst *instance.Instance, target *FileDocWithRevisions, newdoc *vfs.FileDoc, body io.ReadCloser) error {
	rev := target.Rev()
	inst.Logger().WithNamespace("upload").Debugf("uploadLostConflict %s", rev)
	indexer := newSharingIndexer(inst, &bulkRevs{
		Rev:       rev,
		Revisions: revsChainToStruct([]string{rev}),
	}, nil)
	fs := inst.VFS().UseSharingIndexer(indexer)
	newdoc.DocID = conflictID(newdoc.DocID, rev)
	if _, err := fs.FileByID(newdoc.DocID); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		body.Close()
		return nil
	}
	newdoc.DocName = conflictName(indexer, newdoc.DirID, newdoc.DocName, true)
	newdoc.DocRev = ""
	newdoc.ResetFullpath()
	file, err := fs.CreateFile(newdoc, nil)
	if err != nil {
		return err
	}
	inst.Logger().WithNamespace("upload").Debugf("1. loser = %#v", newdoc)
	return copyFileContent(inst, file, body)
}

// uploadWonConflict manages an upload where a file is in conflict, and the
// existing file is copied to a new file to let the upload succeed.
func (s *Sharing) uploadWonConflict(inst *instance.Instance, src *vfs.FileDoc) error {
	rev := src.Rev()
	inst.Logger().WithNamespace("upload").Debugf("uploadWonConflict %s", rev)
	indexer := newSharingIndexer(inst, &bulkRevs{
		Rev:       rev,
		Revisions: revsChainToStruct([]string{rev}),
	}, nil)
	fs := inst.VFS().UseSharingIndexer(indexer)
	dst := src.Clone().(*vfs.FileDoc)
	dst.DocID = conflictID(dst.DocID, rev)
	if _, err := fs.FileByID(dst.DocID); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dst.DocName = conflictName(indexer, dst.DirID, dst.DocName, true)
	dst.ResetFullpath()
	content, err := fs.OpenFile(src)
	if err != nil {
		return err
	}
	defer content.Close()
	file, err := fs.CreateFile(dst, nil)
	if err != nil {
		return err
	}
	inst.Logger().WithNamespace("upload").Debugf("2. loser = %#v", dst)
	return copyFileContent(inst, file, content)
}

// copyFileContent will copy the body of the HTTP request to the file, and
// close the file descriptor at the end.
func copyFileContent(inst *instance.Instance, file vfs.File, body io.ReadCloser) error {
	_, err := io.Copy(file, body)
	if cerr := file.Close(); cerr != nil && err == nil {
		err = cerr
		inst.Logger().WithNamespace("upload").
			Infof("Cannot close file descriptor: %s", err)
	}
	return err
}
