package sharing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/client/request"
	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/bitwarden"
	"github.com/cozy/cozy-stack/model/contact"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/permission"
	csettings "github.com/cozy/cozy-stack/model/settings"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/couchdb/revision"
	"github.com/cozy/cozy-stack/pkg/crypto"
	"github.com/cozy/cozy-stack/pkg/jsonapi"
	"github.com/cozy/cozy-stack/pkg/metadata"
	"github.com/cozy/cozy-stack/pkg/prefixer"
	"github.com/cozy/cozy-stack/pkg/realtime"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/labstack/echo/v4"
)

const (
	// StateLen is the number of bytes for the OAuth state parameter
	StateLen = 16
)

// Triggers keep record of which triggers are active
type Triggers struct {
	TrackID     string   `json:"track_id,omitempty"` // Legacy
	TrackIDs    []string `json:"track_ids,omitempty"`
	ReplicateID string   `json:"replicate_id,omitempty"`
	UploadID    string   `json:"upload_id,omitempty"`
}

// Sharing contains all the information about a sharing.
type Sharing struct {
	SID  string `json:"_id,omitempty"`
	SRev string `json:"_rev,omitempty"`

	Triggers    Triggers  `json:"triggers"`
	Active      bool      `json:"active,omitempty"`
	Owner       bool      `json:"owner,omitempty"`
	Open        bool      `json:"open_sharing,omitempty"`
	Description string    `json:"description,omitempty"`
	AppSlug     string    `json:"app_slug"`
	PreviewPath string    `json:"preview_path,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	NbFiles     int       `json:"initial_number_of_files_to_sync,omitempty"`
	Initial     bool      `json:"initial_sync,omitempty"`
	ShortcutID  string    `json:"shortcut_id,omitempty"`
	MovedFrom   string    `json:"moved_from,omitempty"`

	Rules []Rule `json:"rules"`

	// Members[0] is the owner, Members[1...] are the recipients
	Members []Member `json:"members"`

	// On the owner, credentials[i] is associated to members[i+1]
	// On a recipient, there is only credentials[0] (for the owner)
	Credentials []Credentials `json:"credentials,omitempty"`
}

// ID returns the sharing qualified identifier
func (s *Sharing) ID() string { return s.SID }

// Rev returns the sharing revision
func (s *Sharing) Rev() string { return s.SRev }

// DocType returns the sharing document type
func (s *Sharing) DocType() string { return consts.Sharings }

// SetID changes the sharing qualified identifier
func (s *Sharing) SetID(id string) { s.SID = id }

// SetRev changes the sharing revision
func (s *Sharing) SetRev(rev string) { s.SRev = rev }

// Clone implements couchdb.Doc
func (s *Sharing) Clone() couchdb.Doc {
	cloned := *s
	cloned.Rules = make([]Rule, len(s.Rules))
	copy(cloned.Rules, s.Rules)
	for i := range cloned.Rules {
		cloned.Rules[i].Values = make([]string, len(s.Rules[i].Values))
		copy(cloned.Rules[i].Values, s.Rules[i].Values)
	}
	cloned.Members = make([]Member, len(s.Members))
	copy(cloned.Members, s.Members)
	cloned.Credentials = make([]Credentials, len(s.Credentials))
	copy(cloned.Credentials, s.Credentials)
	for i := range s.Credentials {
		if s.Credentials[i].Client != nil {
			cloned.Credentials[i].Client = s.Credentials[i].Client.Clone()
		}
		if s.Credentials[i].AccessToken != nil {
			cloned.Credentials[i].AccessToken = s.Credentials[i].AccessToken.Clone()
		}
		cloned.Credentials[i].XorKey = make([]byte, len(s.Credentials[i].XorKey))
		copy(cloned.Credentials[i].XorKey, s.Credentials[i].XorKey)
	}
	return &cloned
}

// ReadOnlyFlag returns true only if the given instance is declared a read-only
// member of the sharing.
func (s *Sharing) ReadOnlyFlag() bool {
	if !s.Owner {
		for i, m := range s.Members {
			if i == 0 {
				continue // skip owner
			}
			if m.Instance != "" {
				return m.ReadOnly
			}
		}
	}
	return false
}

// ReadOnlyRules returns true if the rules forbid that a change on the
// recipient's cozy instance can be propagated to the sharer's cozy.
func (s *Sharing) ReadOnlyRules() bool {
	for _, rule := range s.Rules {
		if rule.HasSync() {
			return false
		}
	}
	return true
}

// ReadOnly returns true if the member has the read-only flag, or if the rules
// forces a read-only mode.
func (s *Sharing) ReadOnly() bool {
	return s.ReadOnlyFlag() || s.ReadOnlyRules()
}

// BeOwner initializes a sharing on the cozy of its owner
func (s *Sharing) BeOwner(inst *instance.Instance, slug string) error {
	s.Active = true
	s.Owner = true
	if s.AppSlug == "" {
		s.AppSlug = slug
	}
	if s.AppSlug == "" {
		s.PreviewPath = ""
	}
	s.CreatedAt = time.Now()
	s.UpdatedAt = s.CreatedAt

	name, err := csettings.PublicName(inst)
	if err != nil {
		return err
	}
	email, err := inst.SettingsEMail()
	if err != nil {
		return err
	}

	s.Members = make([]Member, 1)
	s.Members[0].Status = MemberStatusOwner
	s.Members[0].PublicName = name
	s.Members[0].Email = email
	s.Members[0].Instance = inst.PageURL("", nil)

	return nil
}

// CreatePreviewPermissions creates the permissions doc for previewing this sharing,
// or updates it with the new codes if the document already exists
func (s *Sharing) CreatePreviewPermissions(inst *instance.Instance) (*permission.Permission, error) {
	doc, _ := permission.GetForSharePreview(inst, s.SID)

	codes := make(map[string]string, len(s.Members)-1)
	shortcodes := make(map[string]string, len(s.Members)-1)

	for i, m := range s.Members {
		if i == 0 {
			continue
		}
		var err error
		var previousCode, previousShort string
		var okCode, okShort bool
		key := m.Email
		if key == "" {
			key = m.Instance
		}
		if key == "" {
			key = keyFromMemberIndex(i)
		}

		// Checks that we don't already have a sharing code
		if doc != nil {
			previousCode, okCode = doc.Codes[key]
			previousShort, okShort = doc.ShortCodes[key]
		}

		if !okCode {
			codes[key], err = inst.CreateShareCode(key)
			if err != nil {
				return nil, err
			}
		} else {
			codes[key] = previousCode
		}
		if !okShort {
			shortcodes[key] = crypto.GenerateRandomString(consts.ShortCodeLen)
		} else {
			shortcodes[key] = previousShort
		}
	}

	set := make(permission.Set, len(s.Rules))
	getVerb := permission.VerbSplit("GET")
	for i, rule := range s.Rules {
		set[i] = permission.Rule{
			Type:     rule.DocType,
			Title:    rule.Title,
			Verbs:    getVerb,
			Selector: rule.Selector,
			Values:   rule.Values,
		}
	}

	if doc == nil {
		md := metadata.New()
		md.CreatedByApp = s.AppSlug
		subdoc := permission.Permission{
			Permissions: set,
			Metadata:    md,
		}
		return permission.CreateSharePreviewSet(inst, s.SID, codes, shortcodes, subdoc)
	}

	if doc.Metadata != nil {
		err := doc.Metadata.UpdatedByApp(s.AppSlug, "")
		if err != nil {
			return nil, err
		}
	}
	doc.Codes = codes
	doc.ShortCodes = shortcodes
	if err := couchdb.UpdateDoc(inst, doc); err != nil {
		return nil, err
	}
	return doc, nil
}

func keyFromMemberIndex(index int) string {
	return fmt.Sprintf("index:%d", index)
}

// GetInteractCode returns a sharecode that can be used for reading and writing
// the file. It uses a share-interact token.
func (s *Sharing) GetInteractCode(inst *instance.Instance, member *Member, memberIndex int) (string, error) {
	interact, err := permission.GetForShareInteract(inst, s.ID())
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			return s.CreateInteractPermissions(inst, member)
		}
		return "", err
	}

	// Check if the sharing has not been revoked and accepted again, in which
	// case, we need to update the permission set.
	needUpdate := false
	set := s.CreateInteractSet()
	if !set.HasSameRules(interact.Permissions) {
		interact.Permissions = set
		needUpdate = true
	}

	// If we already have a code for this member, let's use it
	indexKey := keyFromMemberIndex(memberIndex)
	for key, code := range interact.Codes {
		if key == "" {
			continue
		}
		if key == member.Instance || key == member.Email || key == indexKey {
			if needUpdate {
				if err := couchdb.UpdateDoc(inst, interact); err != nil {
					return "", err
				}
			}
			return code, nil
		}
	}

	// Else, create a code and add it to the permission doc
	key := member.Email
	if key == "" {
		key = member.Instance
	}
	if key == "" {
		key = indexKey
	}
	code, err := inst.CreateShareCode(key)
	if err != nil {
		return "", err
	}
	interact.Codes[key] = code
	if err := couchdb.UpdateDoc(inst, interact); err != nil {
		return "", err
	}
	return code, nil
}

// CreateInteractPermissions creates the permissions doc for reading and
// writing a note inside this sharing.
func (s *Sharing) CreateInteractPermissions(inst *instance.Instance, m *Member) (string, error) {
	key := m.Email
	if key == "" {
		key = m.Instance
	}
	code, err := inst.CreateShareCode(key)
	if err != nil {
		return "", err
	}
	codes := map[string]string{key: code}
	set := s.CreateInteractSet()

	md := metadata.New()
	md.CreatedByApp = s.AppSlug
	doc := permission.Permission{
		Permissions: set,
		Metadata:    md,
	}

	_, err = permission.CreateShareInteractSet(inst, s.SID, codes, doc)
	if err != nil {
		return "", err
	}
	return code, nil
}

// CreateInteractSet returns a set of permissions that can be used for
// share-interact.
func (s *Sharing) CreateInteractSet() permission.Set {
	set := make(permission.Set, len(s.Rules))
	getVerb := permission.ALL
	for i, rule := range s.Rules {
		set[i] = permission.Rule{
			Type:     rule.DocType,
			Title:    rule.Title,
			Verbs:    getVerb,
			Selector: rule.Selector,
			Values:   rule.Values,
		}
	}
	return set
}

// Create checks that the sharing is OK and it persists it in CouchDB if it is the case.
func (s *Sharing) Create(inst *instance.Instance) (*permission.Permission, error) {
	if err := s.ValidateRules(); err != nil {
		return nil, err
	}
	if len(s.Members) < 2 {
		return nil, ErrNoRecipients
	}

	if err := couchdb.CreateDoc(inst, s); err != nil {
		return nil, err
	}
	if rule := s.FirstFilesRule(); rule != nil && rule.Selector != couchdb.SelectorReferencedBy {
		if err := s.AddReferenceForSharingDir(inst, rule); err != nil {
			inst.Logger().WithNamespace("sharing").
				Warnf("Error on referenced_by for the sharing dir (%s): %s", s.SID, err)
		}
	}

	if s.Owner && s.PreviewPath != "" {
		return s.CreatePreviewPermissions(inst)
	}
	return nil, nil
}

// CreateRequest prepares a sharing as just a request that the user will have to
// accept before it does anything.
func (s *Sharing) CreateRequest(inst *instance.Instance) error {
	if err := s.ValidateRules(); err != nil {
		return err
	}
	if len(s.Members) < 2 {
		return ErrNoRecipients
	}

	s.Active = false
	s.Owner = false
	s.UpdatedAt = time.Now()
	s.Credentials = make([]Credentials, 1)

	for i, m := range s.Members {
		if m.Email != "" {
			if c, err := contact.FindByEmail(inst, m.Email); err == nil {
				s.Members[i].Name = c.PrimaryName()
			}
		}
	}

	err := couchdb.CreateNamedDocWithDB(inst, s)
	if couchdb.IsConflictError(err) {
		old, errb := FindSharing(inst, s.SID)
		if errb != nil {
			return errb
		}
		if old.Owner {
			return ErrInvalidSharing
		}
		if old.Active {
			return ErrAlreadyAccepted
		}
		s.ShortcutID = old.ShortcutID
		s.SRev = old.SRev
		err = couchdb.UpdateDoc(inst, s)
	}
	return err
}

// Revoke remove the credentials for all members, contact them, removes the
// triggers and set the active flag to false.
func (s *Sharing) Revoke(inst *instance.Instance) error {
	var errm error

	if !s.Owner {
		return ErrInvalidSharing
	}
	for i := range s.Credentials {
		if err := s.RevokeMember(inst, i+1); err != nil {
			errm = multierror.Append(errm, err)
		}
		if err := s.ClearLastSequenceNumbers(inst, &s.Members[i+1]); err != nil {
			return err
		}
	}
	if err := s.RemoveTriggers(inst); err != nil {
		return err
	}
	if err := RemoveSharedRefs(inst, s.SID); err != nil {
		return err
	}
	if s.PreviewPath != "" {
		if err := s.RevokePreviewPermissions(inst); err != nil {
			return err
		}
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveAllBitwardenMembers(inst, rule.Values[0]); err != nil {
			return err
		}
	}
	s.Active = false
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	return errm
}

// RevokePreviewPermissions ensure that the permissions for the preview page
// are no longer valid.
func (s *Sharing) RevokePreviewPermissions(inst *instance.Instance) error {
	perms, err := permission.GetForSharePreview(inst, s.SID)
	if err != nil {
		return err
	}
	now := time.Now()
	perms.ExpiresAt = &now
	return couchdb.UpdateDoc(inst, perms)
}

// RevokeRecipient revoke only one recipient on the sharer. After that, if the
// sharing has still at least one active member, we keep it as is. Else, we
// desactive the sharing.
func (s *Sharing) RevokeRecipient(inst *instance.Instance, index int) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	if err := s.RevokeMember(inst, index); err != nil {
		return err
	}
	m := &s.Members[index]
	if err := s.ClearLastSequenceNumbers(inst, m); err != nil {
		return err
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveBitwardenMember(inst, m, rule.Values[0]); err != nil {
			return err
		}
	}
	return s.NoMoreRecipient(inst)
}

// RevokeRecipientBySelf revoke the sharing on the recipient side
func (s *Sharing) RevokeRecipientBySelf(inst *instance.Instance, sharingDirTrashed bool) error {
	if s.Owner || len(s.Members) == 0 {
		return ErrInvalidSharing
	}
	if err := s.RevokeOwner(inst); err != nil {
		return err
	}
	if err := s.RemoveTriggers(inst); err != nil {
		return err
	}
	if err := s.ClearLastSequenceNumbers(inst, &s.Members[0]); err != nil {
		return err
	}
	if err := RemoveSharedRefs(inst, s.SID); err != nil {
		inst.Logger().WithNamespace("sharing").
			Warnf("RevokeRecipientBySelf failed to remove shared refs (%s)': %s", s.ID(), err)
	}
	if !sharingDirTrashed {
		if rule := s.FirstFilesRule(); rule != nil && rule.Mime == "" {
			if err := s.RemoveSharingDir(inst); err != nil {
				inst.Logger().WithNamespace("sharing").
					Warnf("RevokeRecipientBySelf failed to delete dir %s: %s", s.ID(), err)
			}
		}
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveBitwardenOrganization(inst, rule.Values[0]); err != nil {
			return err
		}
	}
	s.Active = false

	for i, m := range s.Members {
		if i > 0 && m.Instance != "" {
			s.Members[i].Status = MemberStatusRevoked
			break
		}
	}

	return couchdb.UpdateDoc(inst, s)
}

// RemoveTriggers remove all the triggers associated to this sharing
func (s *Sharing) RemoveTriggers(inst *instance.Instance) error {
	if err := removeSharingTrigger(inst, s.Triggers.TrackID); err != nil {
		return err
	}
	for _, id := range s.Triggers.TrackIDs {
		if err := removeSharingTrigger(inst, id); err != nil {
			return err
		}
	}
	if err := removeSharingTrigger(inst, s.Triggers.ReplicateID); err != nil {
		return err
	}
	if err := removeSharingTrigger(inst, s.Triggers.UploadID); err != nil {
		return err
	}
	s.Triggers = Triggers{}
	return nil
}

func removeSharingTrigger(inst *instance.Instance, triggerID string) error {
	if triggerID != "" {
		err := job.System().DeleteTrigger(inst, triggerID)
		if err != nil && !errors.Is(err, job.ErrNotFoundTrigger) {
			return err
		}
	}
	return nil
}

// RemoveBitwardenOrganization remove the shared bitwarden organization and the
// ciphers inside it. It is called on the recipient instance when the sharing
// is revoked for them.
func (s *Sharing) RemoveBitwardenOrganization(inst *instance.Instance, orgID string) error {
	org := &bitwarden.Organization{}
	if err := couchdb.GetDoc(inst, consts.BitwardenOrganizations, orgID, org); err != nil {
		if couchdb.IsNotFoundError(err) {
			return nil
		}
		return err
	}
	return org.Delete(inst)
}

// RevokeByNotification is called on the recipient side, after a revocation
// performed by the sharer
func (s *Sharing) RevokeByNotification(inst *instance.Instance) error {
	if s.Owner {
		return ErrInvalidSharing
	}
	if err := DeleteOAuthClient(inst, &s.Members[0], &s.Credentials[0]); err != nil {
		return err
	}
	if err := s.RemoveTriggers(inst); err != nil {
		return err
	}
	if err := s.ClearLastSequenceNumbers(inst, &s.Members[0]); err != nil {
		return err
	}
	if err := RemoveSharedRefs(inst, s.SID); err != nil {
		return err
	}
	if rule := s.FirstFilesRule(); rule != nil && rule.Mime == "" {
		if err := s.RemoveSharingDir(inst); err != nil {
			return err
		}
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveBitwardenOrganization(inst, rule.Values[0]); err != nil {
			return err
		}
	}

	var err error
	for i := 0; i < 3; i++ {
		s.Triggers = Triggers{}
		s.Credentials = nil
		s.Active = false

		for i, m := range s.Members {
			if i > 0 && m.Instance != "" {
				s.Members[i].Status = MemberStatusRevoked
				break
			}
		}

		err := couchdb.UpdateDoc(inst, s)
		if err == nil || !couchdb.IsConflictError(err) {
			break
		}

		// In case of conflict (409 from CouchDB), reload the document and try again
		if errb := couchdb.GetDoc(inst, consts.Sharings, s.ID(), s); errb != nil {
			break
		}
	}
	return err
}

// RevokeRecipientByNotification is called on the sharer side, after a
// revocation performed by the recipient
func (s *Sharing) RevokeRecipientByNotification(inst *instance.Instance, m *Member) error {
	if !s.Owner {
		return ErrInvalidSharing
	}
	c := s.FindCredentials(m)
	if err := DeleteOAuthClient(inst, m, c); err != nil {
		return err
	}
	if err := s.ClearLastSequenceNumbers(inst, m); err != nil {
		return err
	}
	if rule := s.FirstBitwardenOrganizationRule(); rule != nil && len(rule.Values) > 0 {
		if err := s.RemoveBitwardenMember(inst, m, rule.Values[0]); err != nil {
			return err
		}
	}
	m.Status = MemberStatusRevoked
	*c = Credentials{}

	return s.NoMoreRecipient(inst)
}

// NoMoreRecipient cleans up the sharing if there is no more active recipient
func (s *Sharing) NoMoreRecipient(inst *instance.Instance) error {
	for _, m := range s.Members {
		if m.Status == MemberStatusReady {
			return couchdb.UpdateDoc(inst, s)
		}
	}
	if err := s.RemoveTriggers(inst); err != nil {
		return err
	}
	s.Active = false
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	return RemoveSharedRefs(inst, s.SID)
}

// FindSharing retrieves a sharing document from its ID
func FindSharing(db prefixer.Prefixer, sharingID string) (*Sharing, error) {
	res := &Sharing{}
	err := couchdb.GetDoc(db, consts.Sharings, sharingID, res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// FindSharings retrieves an array of sharing documents from their IDs
func FindSharings(db prefixer.Prefixer, sharingIDs []string) ([]*Sharing, error) {
	req := &couchdb.AllDocsRequest{
		Keys: sharingIDs,
	}
	var res []*Sharing
	err := couchdb.GetAllDocs(db, consts.Sharings, req, &res)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// GetSharingsByDocType returns all the sharings for the given doctype
func GetSharingsByDocType(inst *instance.Instance, docType string) (map[string]*Sharing, error) {
	req := &couchdb.ViewRequest{
		Key:         docType,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.SharingsByDocTypeView, req, &res)
	if err != nil {
		return nil, err
	}
	sharings := make(map[string]*Sharing, len(res.Rows))

	for _, row := range res.Rows {
		var doc Sharing
		err := json.Unmarshal(row.Doc, &doc)
		if err != nil {
			return nil, err
		}
		// Avoid duplicates, i.e. a set a rules having the same doctype
		sID := row.Value.(string)
		if _, ok := sharings[sID]; !ok {
			sharings[sID] = &doc
		}
	}
	return sharings, nil
}

func findIntentForRedirect(inst *instance.Instance, webapp *app.WebappManifest, doctype string) (*app.Intent, string) {
	action := "SHARING"
	if webapp != nil {
		if intent := webapp.FindIntent(action, doctype); intent != nil {
			return intent, webapp.Slug()
		}
	}
	var mans []app.WebappManifest
	err := couchdb.GetAllDocs(inst, consts.Apps, &couchdb.AllDocsRequest{}, &mans)
	if err != nil {
		return nil, ""
	}
	for _, man := range mans {
		if intent := man.FindIntent(action, doctype); intent != nil {
			return intent, man.Slug()
		}
	}
	return nil, ""
}

// RedirectAfterAuthorizeURL returns the URL for the redirection after a user
// has authorized a sharing.
func (s *Sharing) RedirectAfterAuthorizeURL(inst *instance.Instance) *url.URL {
	doctype := s.Rules[0].DocType
	webapp, _ := app.GetWebappBySlug(inst, s.AppSlug)

	if intent, slug := findIntentForRedirect(inst, webapp, doctype); intent != nil {
		u := inst.SubDomain(slug)
		parts := strings.SplitN(intent.Href, "#", 2)
		if len(parts[0]) > 0 {
			u.Path = parts[0]
		}
		if len(parts) == 2 && len(parts[1]) > 0 {
			u.Fragment = parts[1]
		}
		u.RawQuery = "sharing=" + s.SID
		return u
	}

	if webapp == nil {
		return inst.DefaultRedirection()
	}
	u := inst.SubDomain(webapp.Slug())
	u.RawQuery = "sharing=" + s.SID
	return u
}

// EndInitial is used to finish the initial sync phase of a sharing
func (s *Sharing) EndInitial(inst *instance.Instance) error {
	if s.NbFiles == 0 {
		return nil
	}
	s.NbFiles = 0
	s.Initial = false
	if err := couchdb.UpdateDoc(inst, s); err != nil {
		return err
	}
	doc := couchdb.JSONDoc{
		Type: consts.SharingsInitialSync,
		M:    map[string]interface{}{"_id": s.SID},
	}
	realtime.GetHub().Publish(inst, realtime.EventDelete, &doc, nil)
	return nil
}

// GetSharecode returns a sharecode for the given client that can be used to
// preview the sharing.
func GetSharecode(inst *instance.Instance, sharingID, clientID string) (string, error) {
	var s Sharing
	if err := couchdb.GetDoc(inst, consts.Sharings, sharingID, &s); err != nil {
		return "", err
	}
	member, err := s.FindMemberByInboundClientID(clientID)
	if err != nil {
		return "", err
	}
	preview, err := permission.GetForSharePreview(inst, sharingID)
	if err != nil {
		if couchdb.IsNotFoundError(err) {
			preview, err = s.CreatePreviewPermissions(inst)
		}
		if err != nil {
			return "", err
		}
	}

	for key, code := range preview.ShortCodes {
		if key == member.Instance || key == member.Email {
			return code, nil
		}
	}
	for key, code := range preview.Codes {
		if key == member.Instance || key == member.Email {
			return code, nil
		}
	}
	return "", ErrMemberNotFound
}

var _ couchdb.Doc = &Sharing{}

// GetSharecodeFromShortcut returns the sharecode from the shortcut for this sharing.
func (s *Sharing) GetSharecodeFromShortcut(inst *instance.Instance) (string, error) {
	key := []string{consts.Sharings, s.SID}
	end := []string{key[0], key[1], couchdb.MaxString}
	req := &couchdb.ViewRequest{
		StartKey:    key,
		EndKey:      end,
		IncludeDocs: true,
	}
	var res couchdb.ViewResponse
	err := couchdb.ExecView(inst, couchdb.FilesReferencedByView, req, &res)
	if err != nil {
		return "", ErrInternalServerError
	}
	if len(res.Rows) == 0 {
		return "", ErrInvalidSharing
	}

	fs := inst.VFS()
	file, err := fs.FileByID(res.Rows[0].ID)
	if err != nil || file.Mime != consts.ShortcutMimeType {
		return "", ErrInvalidSharing
	}
	f, err := fs.OpenFile(file)
	if err != nil {
		return "", ErrInternalServerError
	}
	defer f.Close()
	var buf bytes.Buffer
	_, err = buf.ReadFrom(f)
	if err != nil {
		return "", ErrInternalServerError
	}
	u, err := url.Parse(buf.String())
	if err != nil {
		return "", ErrInternalServerError
	}
	code := u.Query().Get("sharecode")
	if code == "" {
		return "", ErrInvalidSharing
	}
	return code, nil
}

func (s *Sharing) cleanShortcutID(inst *instance.Instance) string {
	if s.ShortcutID == "" {
		return ""
	}

	var parentID string
	fs := inst.VFS()
	if file, err := fs.FileByID(s.ShortcutID); err == nil {
		parentID = file.DirID
		if err := fs.DestroyFile(file); err != nil {
			return ""
		}
	}
	s.ShortcutID = ""
	_ = couchdb.UpdateDoc(inst, s)
	return parentID
}

// GetPreviewURL asks the owner's Cozy the URL for previewing the sharing.
func (s *Sharing) GetPreviewURL(inst *instance.Instance, state string) (string, error) {
	u, err := url.Parse(s.Members[0].Instance)
	if s.Members[0].Instance == "" || err != nil {
		return "", ErrInvalidSharing
	}
	body, err := json.Marshal(map[string]interface{}{"state": state})
	if err != nil {
		return "", err
	}
	res, err := request.Req(&request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/preview-url",
		Headers: request.Headers{
			echo.HeaderAccept:      echo.MIMEApplicationJSON,
			echo.HeaderContentType: echo.MIMEApplicationJSON,
		},
		Body: bytes.NewReader(body),
	})
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	var data map[string]interface{}
	if err = json.NewDecoder(res.Body).Decode(&data); err != nil {
		return "", ErrRequestFailed
	}

	previewURL, ok := data["url"].(string)
	if !ok || previewURL == "" {
		return "", ErrRequestFailed
	}
	return previewURL, nil
}

// AddShortcut creates a shortcut for this sharing on the local instance.
func (s *Sharing) AddShortcut(inst *instance.Instance, state string) error {
	previewURL, err := s.GetPreviewURL(inst, state)
	if err != nil {
		return err
	}
	return s.CreateShortcut(inst, previewURL, true)
}

// CountNewShortcuts returns the number of shortcuts to a sharing that have not
// been seen.
func CountNewShortcuts(inst *instance.Instance) (int, error) {
	count := 0
	perPage := 1000
	list := make([]couchdb.JSONDoc, 0, perPage)
	var bookmark string
	for {
		req := &couchdb.FindRequest{
			UseIndex: "by-sharing-status",
			Selector: mango.Equal("metadata.sharing.status", "new"),
			Limit:    perPage,
			Bookmark: bookmark,
		}
		res, err := couchdb.FindDocsRaw(inst, consts.Files, req, &list)
		if err != nil {
			return 0, err
		}
		count += len(list)
		if len(list) < perPage {
			return count, nil
		}
		bookmark = res.Bookmark
	}
}

// SendPublicKey can be used to send the public key after it has been
// created/changed to the sharing owners.
func SendPublicKey(inst *instance.Instance, publicKey string) error {
	sharings, err := GetSharingsByDocType(inst, consts.BitwardenOrganizations)
	if err != nil {
		return err
	}
	var errm error
	for _, s := range sharings {
		if s.Owner || !s.Active || s.Credentials == nil {
			continue
		}
		if err := s.sendPublicKeyToOwner(inst, publicKey); err != nil {
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

func (s *Sharing) sendPublicKeyToOwner(inst *instance.Instance, publicKey string) error {
	u, err := url.Parse(s.Members[0].Instance)
	if err != nil {
		return err
	}
	ac := APICredentials{
		Bitwarden: &APIBitwarden{
			UserID:    inst.ID(),
			PublicKey: publicKey,
		},
	}
	data, err := jsonapi.MarshalObject(&ac)
	if err != nil {
		return err
	}
	body, err := json.Marshal(jsonapi.Document{Data: &data})
	if err != nil {
		return err
	}
	opts := &request.Options{
		Method: http.MethodPost,
		Scheme: u.Scheme,
		Domain: u.Host,
		Path:   "/sharings/" + s.SID + "/public-key",
		Headers: request.Headers{
			echo.HeaderContentType:   jsonapi.ContentType,
			echo.HeaderAuthorization: "Bearer " + s.Credentials[0].AccessToken.AccessToken,
		},
		Body:       bytes.NewReader(body),
		ParseError: ParseRequestError,
	}
	res, err := request.Req(opts)
	if res != nil && res.StatusCode/100 == 4 {
		res, err = RefreshToken(inst, err, s, &s.Members[0], &s.Credentials[0], opts, body)
	}
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, res.Body)
	res.Body.Close()
	return nil
}

// CheckSharings will scan all the io.cozy.sharings documents and check their
// triggers and members/credentials.
func CheckSharings(inst *instance.Instance, skipFSConsistency bool) ([]map[string]interface{}, error) {
	checks := []map[string]interface{}{}
	err := couchdb.ForeachDocs(inst, consts.Sharings, func(_ string, data json.RawMessage) error {
		s := &Sharing{}
		if err := json.Unmarshal(data, s); err != nil {
			return err
		}

		if err := s.ValidateRules(); err != nil {
			checks = append(checks, map[string]interface{}{
				"id":    s.SID,
				"type":  "invalid_rules",
				"error": err.Error(),
			})
			return nil
		}

		accepted := false
		for _, m := range s.Members {
			if m.Status == MemberStatusReady {
				accepted = true
			}
		}

		membersChecks, validMembers := s.checkSharingMembers()
		checks = append(checks, membersChecks...)

		triggersChecks := s.checkSharingTriggers(inst, accepted)
		checks = append(checks, triggersChecks...)

		credentialsChecks := s.checkSharingCredentials()
		checks = append(checks, credentialsChecks...)

		if len(membersChecks) == 0 && len(triggersChecks) == 0 && len(credentialsChecks) == 0 {
			if !s.Owner || !s.Active {
				return nil
			}

			parentSharingID, err := findParentFileSharingID(inst, s)
			if err != nil {
				return err
			} else if parentSharingID != "" {
				checks = append(checks, map[string]interface{}{
					"id":             s.SID,
					"type":           "sharing_in_sharing",
					"instance":       inst.Domain,
					"parent_sharing": parentSharingID,
				})
				return nil
			}

			if s.Initial || s.ReadOnly() {
				return nil
			}

			isSharingReady := false
			for _, m := range s.Members {
				if m.Status == MemberStatusReady {
					isSharingReady = true
					break
				}
			}
			if !isSharingReady {
				return nil
			}

			rule := s.FirstFilesRule()
			if rule == nil {
				return nil
			}

			ownerDocs, err := FindMatchingDocs(inst, *rule)
			if err != nil {
				checks = append(checks, map[string]interface{}{
					"id":    s.SID,
					"type":  "missing_matching_docs_for_owner",
					"error": err.Error(),
				})
				return nil
			}

			for _, m := range validMembers {
				ms, err := FindSharing(m, s.ID())
				if err != nil {
					checks = append(checks, map[string]interface{}{
						"id":     s.SID,
						"type":   "missing_sharing_for_member",
						"member": m.Domain,
						"error":  err.Error(),
					})
					continue
				}

				if !ms.Active {
					continue
				}

				parentSharingID, err := findParentFileSharingID(m, ms)
				if err != nil {
					return err
				} else if parentSharingID != "" {
					checks = append(checks, map[string]interface{}{
						"id":             ms.SID,
						"type":           "sharing_in_sharing",
						"instance":       m.Domain,
						"parent_sharing": parentSharingID,
					})
					continue
				}

				if !skipFSConsistency {
					checks = append(checks, s.checkSharingTreesConsistency(inst, ownerDocs, m, ms)...)
				}
			}
		}

		return nil
	})
	return checks, err
}

// findParentFileSharingID returns the first sharing found accepting the root of
// the given sharing.
//
// Since have a sharing within another one will generate unexpected behavior,
// the goal is to find these situations.
func findParentFileSharingID(inst *instance.Instance, sharing *Sharing) (string, error) {
	// Inactive sharings are not an issue so we skip them
	if !sharing.Active {
		return "", nil
	}

	// 1. Get all root files for the sharing being checked
	sharingRule := sharing.FirstFilesRule()
	if sharingRule == nil {
		return "", nil
	}

	var sharingRoots []couchdb.JSONDoc
	for _, id := range sharingRule.Values {
		var sharingRoot couchdb.JSONDoc
		if err := couchdb.GetDoc(inst, consts.Files, id, &sharingRoot); err != nil {
			// We can ignore the error here. It will be reported as
			// missing_matching_docs_for_owner or missing_matching_docs_for_member
			// later.
			return "", nil
		}
		sharingRoots = append(sharingRoots, sharingRoot)
	}

	// 2. Get all other file sharings on inst
	fileSharings, err := GetSharingsByDocType(inst, consts.Files)
	if err != nil {
		return "", err
	}

	var sharingIDs []string
	for _, fileSharing := range fileSharings {
		// Do not add sharing in sharing error for inactive sharings or the
		// sharing currently checked.
		if !fileSharing.Active || fileSharing.ID() == sharing.ID() {
			continue
		}

		sharingIDs = append(sharingIDs, fileSharing.ID())
	}

	sharedDocsBySharingID, err := GetSharedDocsBySharingIDs(inst, sharingIDs)
	if err != nil {
		return "", err
	}

	// 3. Check if one of the shared roots is part of another sharing
	for _, sharedRoot := range sharingRoots {
		for sid, sharedDocs := range sharedDocsBySharingID {
			for _, sharedDoc := range sharedDocs {
				if sharedRoot.ID() == sharedDoc.ID {
					return sid, nil
				}
			}
		}
	}

	return "", nil
}

func (s *Sharing) checkSharingTriggers(inst *instance.Instance, accepted bool) (checks []map[string]interface{}) {
	if s.Active && accepted {
		if s.Triggers.TrackID == "" && len(s.Triggers.TrackIDs) == 0 {
			checks = append(checks, map[string]interface{}{
				"id":      s.SID,
				"type":    "missing_trigger_on_active_sharing",
				"trigger": "track",
			})
		} else if s.Triggers.TrackID != "" {
			err := couchdb.GetDoc(inst, consts.Triggers, s.Triggers.TrackID, nil)
			if couchdb.IsNotFoundError(err) {
				checks = append(checks, map[string]interface{}{
					"id":         s.SID,
					"type":       "missing_trigger_on_active_sharing",
					"trigger":    "track",
					"trigger_id": s.Triggers.TrackID,
				})
			}
		} else {
			for _, id := range s.Triggers.TrackIDs {
				err := couchdb.GetDoc(inst, consts.Triggers, id, nil)
				if couchdb.IsNotFoundError(err) {
					checks = append(checks, map[string]interface{}{
						"id":         s.SID,
						"type":       "missing_trigger_on_active_sharing",
						"trigger":    "track",
						"trigger_id": id,
					})
				}
			}
		}

		if s.Owner || !s.ReadOnly() {
			if s.Triggers.ReplicateID == "" {
				checks = append(checks, map[string]interface{}{
					"id":      s.SID,
					"type":    "missing_trigger_on_active_sharing",
					"trigger": "replicate",
				})
			} else {
				err := couchdb.GetDoc(inst, consts.Triggers, s.Triggers.ReplicateID, nil)
				if couchdb.IsNotFoundError(err) {
					checks = append(checks, map[string]interface{}{
						"id":         s.SID,
						"type":       "missing_trigger_on_active_sharing",
						"trigger":    "replicate",
						"trigger_id": s.Triggers.ReplicateID,
					})
				}
			}

			if s.FirstFilesRule() != nil {
				if s.Triggers.UploadID == "" {
					checks = append(checks, map[string]interface{}{
						"id":      s.SID,
						"type":    "missing_trigger_on_active_sharing",
						"trigger": "upload",
					})
				} else {
					err := couchdb.GetDoc(inst, consts.Triggers, s.Triggers.UploadID, nil)
					if couchdb.IsNotFoundError(err) {
						checks = append(checks, map[string]interface{}{
							"id":         s.SID,
							"type":       "missing_trigger_on_active_sharing",
							"trigger":    "upload",
							"trigger_id": s.Triggers.UploadID,
						})
					}
				}
			}
		}
	} else {
		if s.Triggers.TrackID != "" || len(s.Triggers.TrackIDs) > 0 {
			id := s.Triggers.TrackID
			if id == "" {
				id = s.Triggers.TrackIDs[0]
			}
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "trigger_on_inactive_sharing",
				"trigger":    "track",
				"trigger_id": id,
			})
		}
		if s.Triggers.ReplicateID != "" {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "trigger_on_inactive_sharing",
				"trigger":    "replicate",
				"trigger_id": s.Triggers.ReplicateID,
			})
		}
		if s.Triggers.UploadID != "" {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "trigger_on_inactive_sharing",
				"trigger":    "upload",
				"trigger_id": s.Triggers.UploadID,
			})
		}
	}

	return checks
}

func (s *Sharing) checkSharingMembers() (checks []map[string]interface{}, validMembers []*instance.Instance) {
	if len(s.Members) < 2 {
		checks = append(checks, map[string]interface{}{
			"id":         s.SID,
			"type":       "not_enough_members",
			"nb_members": len(s.Members),
		})
		return checks, nil
	}

	var ownerDomain string
	for i, m := range s.Members {
		if m.Status == MemberStatusRevoked && !s.Active {
			continue
		}

		isFirst := i == 0
		isOwner := m.Status == MemberStatusOwner

		if isFirst != isOwner {
			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "invalid_member_status",
				"member": i,
				"status": m.Status,
			})
		}

		if isOwner {
			ownerDomain = strings.SplitN(m.Instance, ".", 2)[1]
		}
	}

	for _, m := range s.Members {
		if m.Status == MemberStatusMailNotSent {
			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "mail_not_sent",
				"member": m.Instance,
			})
			continue
		}

		if m.Status != MemberStatusReady {
			continue
		}

		u, err := url.ParseRequestURI(m.Instance)
		if err != nil {
			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "invalid_instance_for_member",
				"member": m.Instance,
			})
			continue
		}

		domain := strings.ToLower(u.Hostname())
		if u.Port() != "" {
			domain += ":" + u.Port()
		}

		member, err := instance.Get(domain)
		if err != nil {
			// If the member's instance cannot be found and doesn't share the
			// owner's instance domain, they're probably on different
			// environments so we simply skip this member.
			if !strings.HasSuffix(m.Instance, ownerDomain) {
				continue
			}

			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "missing_instance_for_member",
				"member": domain,
			})
			continue
		}

		validMembers = append(validMembers, member)
	}

	return checks, validMembers
}

func (s *Sharing) checkSharingCredentials() (checks []map[string]interface{}) {
	if !s.Active {
		return checks
	}

	if s.Owner {
		for i, m := range s.Members {
			if i == 0 || m.Status != MemberStatusReady {
				continue
			}
			if s.Credentials[i-1].Client == nil {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "missing_oauth_client",
					"member": i,
					"owner":  true,
				})
			}
			if s.Credentials[i-1].AccessToken == nil {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "missing_access_token",
					"member": i,
					"owner":  true,
				})
			}
			if m.Instance == "" {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "missing_instance_for_member",
					"member": i,
				})
			}
		}

		if len(s.Credentials)+1 != len(s.Members) {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "invalid_number_of_credentials",
				"owner":      true,
				"nb_members": len(s.Credentials),
			})
			return checks
		}
	} else {
		if len(s.Credentials) != 1 {
			checks = append(checks, map[string]interface{}{
				"id":         s.SID,
				"type":       "invalid_number_of_credentials",
				"owner":      false,
				"nb_members": len(s.Credentials),
			})
			return checks
		}

		if s.Credentials[0].InboundClientID == "" {
			checks = append(checks, map[string]interface{}{
				"id":    s.SID,
				"type":  "missing_inbound_client_id",
				"owner": false,
			})
		}

		if !s.ReadOnly() && s.Members[0].Instance == "" {
			checks = append(checks, map[string]interface{}{
				"id":     s.SID,
				"type":   "missing_instance_for_member",
				"member": 0,
			})
		}
	}

	return checks
}

func (s *Sharing) checkSharingTreesConsistency(inst *instance.Instance, ownerDocs []couchdb.JSONDoc, m *instance.Instance, ms *Sharing) (checks []map[string]interface{}) {
	// We checked earlier that this rule exists
	ownerRule := s.FirstFilesRule()

	memberRule := ms.FirstFilesRule()
	if memberRule == nil {
		checks = append(checks, map[string]interface{}{
			"id":     s.SID,
			"type":   "missing_files_rule_for_member",
			"member": m.Domain,
		})
		return checks
	}

	memberDocs, err := FindMatchingDocs(m, *memberRule)
	if err != nil {
		checks = append(checks, map[string]interface{}{
			"id":     s.SID,
			"type":   "missing_matching_docs_for_member",
			"member": m.Domain,
			"error":  err.Error(),
		})
		return checks
	}

	if len(ms.Credentials) != 1 {
		checks = append(checks, map[string]interface{}{
			"id":         s.SID,
			"type":       "invalid_number_of_credentials",
			"instance":   m.Domain,
			"nb_members": len(ms.Credentials),
		})
		return checks
	}

	// Build a map of owner docs with their member's counterpart ids
	ownerKey := ms.Credentials[0].XorKey
	ownerDocsById := make(map[string]couchdb.JSONDoc)
	for _, doc := range ownerDocs {
		ownerDocsById[doc.ID()] = doc
	}

	for _, memberDoc := range memberDocs {
		ownerID := XorID(memberDoc.ID(), ownerKey)

		if ownerDoc, found := ownerDocsById[ownerID]; found {
			if ownerDoc.Rev() != memberDoc.Rev() {
				if revision.Generation(ownerDoc.Rev()) < revision.Generation(memberDoc.Rev()) && ms.ReadOnly() {
					checks = append(checks, map[string]interface{}{
						"id":     s.SID,
						"type":   "read_only_member",
						"member": m.Domain,
					})
				} else if wasUpdatedRecently(ownerDoc) || wasUpdatedRecently(memberDoc) {
					// If the latest change happened less than 5 minutes ago, we'll
					// assume the sharing synchronization is still in progress and
					// that would explain the difference between the 2 revisions.
					// In this case, we do nothing.
				} else if revision.Generation(ownerDoc.Rev()) > revision.Generation(memberDoc.Rev()) && isFileTooBigForInstance(m, ownerDoc) {
					checks = append(checks, map[string]interface{}{
						"id":       s.SID,
						"type":     "disk_quota_exceeded",
						"instance": m.Domain,
						"file":     ownerDoc,
					})
				} else if revision.Generation(ownerDoc.Rev()) < revision.Generation(memberDoc.Rev()) && isFileTooBigForInstance(inst, memberDoc) {
					checks = append(checks, map[string]interface{}{
						"id":       s.SID,
						"type":     "disk_quota_exceeded",
						"instance": inst.Domain,
						"file":     memberDoc,
					})
				} else {
					checks = append(checks, map[string]interface{}{
						"id":        s.SID,
						"type":      "invalid_doc_rev",
						"member":    m.Domain,
						"ownerDoc":  ownerDoc,
						"memberRev": memberDoc.Rev(),
					})
				}
			} else {
				// It's unnecessary to run these checks if both docs don't
				// have the same revision in the first place.

				if ownerDoc.M["name"] != memberDoc.M["name"] {
					checks = append(checks, map[string]interface{}{
						"id":         s.SID,
						"type":       "invalid_doc_name",
						"member":     m.Domain,
						"ownerDoc":   ownerDoc,
						"memberName": memberDoc.M["name"],
					})
				}

				if ownerDoc.M["type"] == consts.FileType && ownerDoc.M["checksum"] != memberDoc.M["checksum"] {
					checks = append(checks, map[string]interface{}{
						"id":             s.SID,
						"type":           "invalid_doc_checksum",
						"member":         m.Domain,
						"ownerDoc":       ownerDoc,
						"memberChecksum": memberDoc.M["checksum"],
					})
				}

				isSharingRoot := false
				for _, v := range ownerRule.Values {
					if ownerDoc.ID() == v {
						isSharingRoot = true
						break
					}
				}

				// Sharing roots are expected not to have the same parent
				if !isSharingRoot {
					memberDirID := memberDoc.M["dir_id"].(string)
					ownerDirID := ownerDoc.M["dir_id"].(string)
					if ownerDirID != XorID(memberDirID, ownerKey) {
						checks = append(checks, map[string]interface{}{
							"id":           s.SID,
							"type":         "invalid_doc_parent",
							"member":       m.Domain,
							"ownerDoc":     ownerDoc,
							"memberParent": memberDirID,
						})
					}
				}
			}

			delete(ownerDocsById, ownerID)
		} else {
			if ms.ReadOnly() {
				checks = append(checks, map[string]interface{}{
					"id":     s.SID,
					"type":   "read_only_member",
					"member": m.Domain,
				})
				continue
			}

			if wasUpdatedRecently(memberDoc) {
				// If the document was created less than 5 minutes ago, we'll
				// assume the sharing synchronization is still in progress and
				// that would explain why it's missing on the other instance.
				// In this case, we do nothing.
				continue
			}

			if isFileTooBigForInstance(inst, memberDoc) {
				checks = append(checks, map[string]interface{}{
					"id":       s.SID,
					"type":     "disk_quota_exceeded",
					"instance": inst.Domain,
					"file":     memberDoc,
				})
				continue
			}

			checks = append(checks, map[string]interface{}{
				"id":      s.SID,
				"type":    "missing_matching_doc_for_owner",
				"member":  m.Domain,
				"missing": memberDoc,
			})
		}
	}

	// The only docs left in the map do not exist on the member's instance
	for _, ownerDoc := range ownerDocsById {
		if wasUpdatedRecently(ownerDoc) {
			// If the document was created less than 5 minutes ago, we'll
			// assume the sharing synchronization is still in progress and
			// that would explain why it's missing on the other instance.
			// In this case, we do nothing.
			continue
		}

		if isFileTooBigForInstance(m, ownerDoc) {
			checks = append(checks, map[string]interface{}{
				"id":       s.SID,
				"type":     "disk_quota_exceeded",
				"instance": m.Domain,
				"file":     ownerDoc,
			})
			break
		}

		checks = append(checks, map[string]interface{}{
			"id":         s.SID,
			"type":       "missing_matching_doc_for_member",
			"member":     m.Domain,
			"missing":    ownerDoc,
			"ownerDocID": ownerDoc.ID(),
		})
	}

	return checks
}

// isFileTooBigForInstance returns true if the given doc represents a file and
// its size is greater than the available space on the given instance.
// If said instance does not have any defined quota, it returns false.
func isFileTooBigForInstance(inst *instance.Instance, doc couchdb.JSONDoc) bool {
	if docType, ok := doc.M["type"].(string); !ok || docType == "" || docType == consts.DirType {
		return false
	}

	var file *vfs.FileDoc

	fileJSON, err := json.Marshal(doc)
	if err != nil {
		return false
	}

	if err := json.Unmarshal(fileJSON, &file); err != nil {
		return false
	}

	_, _, _, err = vfs.CheckAvailableDiskSpace(inst.VFS(), file)
	return errors.Is(err, vfs.ErrFileTooBig) || errors.Is(err, vfs.ErrMaxFileSize)
}

// wasUpdatedRecently returns true if the given document's latest update, given
// by its `cozyMetadata.updatedAt` attribute, happened less than 5 minutes ago.
// If the attribute is missing or does not represent a valid date, we consider
// the latest update happened before that.
func wasUpdatedRecently(doc couchdb.JSONDoc) bool {
	cozyMetadata, ok := doc.M["cozyMetadata"].(map[string]interface{})
	if !ok || cozyMetadata == nil {
		return false
	}
	if updatedAt, ok := cozyMetadata["updatedAt"].(time.Time); ok {
		return time.Since(updatedAt) < 5*time.Minute
	}
	return false
}
