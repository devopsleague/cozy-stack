package center

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cozy/cozy-stack/model/app"
	"github.com/cozy/cozy-stack/model/instance"
	"github.com/cozy/cozy-stack/model/instance/lifecycle"
	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/model/notification"
	"github.com/cozy/cozy-stack/model/oauth"
	"github.com/cozy/cozy-stack/model/permission"
	"github.com/cozy/cozy-stack/model/vfs"
	"github.com/cozy/cozy-stack/pkg/consts"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/couchdb/mango"
	"github.com/cozy/cozy-stack/pkg/mail"
	multierror "github.com/hashicorp/go-multierror"
)

const (
	// NotificationDiskQuota category for sending alert when reaching 90% of disk
	// usage quota.
	NotificationDiskQuota = "disk-quota"
	// NotificationOAuthClients category for sending alert when exceeding the
	// connected OAuth clients limit.
	NotificationOAuthClients = "oauth-clients"
)

var (
	stackNotifications = map[string]*notification.Properties{
		NotificationDiskQuota: {
			Description:  "Warn about the diskquota reaching a high level",
			Collapsible:  true,
			Stateful:     true,
			MailTemplate: "notifications_diskquota",
			MinInterval:  7 * 24 * time.Hour,
		},
		NotificationOAuthClients: {
			Description:  "Warn about the connected OAuth clients count exceeding the offer limit",
			Collapsible:  false,
			Stateful:     false,
			MailTemplate: "notifications_oauthclients",
		},
	}
)

func init() {
	vfs.RegisterDiskQuotaAlertCallback(func(domain string, capsizeExceeded bool) {
		i, err := lifecycle.GetInstance(domain)
		if err != nil {
			return
		}

		title := i.Translate("Notifications Disk Quota Close Title")
		message := i.Translate("Notifications Disk Quota Close Message")
		offersLink, err := i.ManagerURL(instance.ManagerPremiumURL)
		if err != nil {
			return
		}
		cozyDriveLink := i.SubDomain(consts.DriveSlug)
		redirectLink := consts.SettingsSlug + "/#/storage"

		n := &notification.Notification{
			Title:   title,
			Message: message,
			Slug:    consts.SettingsSlug,
			State:   capsizeExceeded,
			Data: map[string]interface{}{
				// For email notification
				"OffersLink":    offersLink,
				"CozyDriveLink": cozyDriveLink.String(),

				// For mobile push notification
				"appName":      "",
				"redirectLink": redirectLink,
			},
			PreferredChannels: []string{"mobile"},
		}
		_ = PushStack(domain, NotificationDiskQuota, n)
	})

	oauth.RegisterClientsLimitAlertCallback(func(i *instance.Instance, clientName string, clientsLimit int) {
		devicesLink := i.SubDomain(consts.SettingsSlug)
		devicesLink.Fragment = "/connectedDevices"

		var offersLink string
		if i.HasPremiumLinksEnabled() {
			var err error
			offersLink, err = i.ManagerURL(instance.ManagerPremiumURL)
			if err != nil {
				i.Logger().Errorf("Could not get instance Premium Manager URL: %s", err.Error())
			}
		}

		n := &notification.Notification{
			Title: i.Translate("Notifications OAuth Clients Subject"),
			Slug:  consts.SettingsSlug,
			Data: map[string]interface{}{
				"ClientName":   clientName,
				"ClientsLimit": clientsLimit,
				"OffersLink":   offersLink,
				"DevicesLink":  devicesLink.String(),
			},
			PreferredChannels: []string{"mail"},
		}
		PushStack(i.DomainName(), NotificationOAuthClients, n)
	})
}

// PushStack creates and sends a new notification where the source is the stack.
func PushStack(domain string, category string, n *notification.Notification) error {
	inst, err := lifecycle.GetInstance(domain)
	if err != nil {
		return err
	}
	n.Originator = "stack"
	n.Category = category
	p := stackNotifications[category]
	if p == nil {
		return ErrCategoryNotFound
	}
	return makePush(inst, p, n)
}

// Push creates and sends a new notification in database. This method verifies
// the permissions associated with this creation in order to check that it is
// granted to create a notification and to extract its source.
func Push(inst *instance.Instance, perm *permission.Permission, n *notification.Notification) error {
	if n.Title == "" {
		return ErrBadNotification
	}

	var p notification.Properties
	switch perm.Type {
	case permission.TypeOauth:
		c, ok := perm.Client.(*oauth.Client)
		if !ok {
			return ErrUnauthorized
		}
		n.Slug = ""
		if slug := oauth.GetLinkedAppSlug(c.SoftwareID); slug != "" {
			n.Slug = slug
			m, err := app.GetWebappBySlug(inst, slug)
			if err != nil {
				return err
			}
			notifications := m.Notifications()
			if notifications == nil {
				return ErrNoCategory
			}
			p, ok = notifications[n.Category]
		} else if c.Notifications != nil {
			p, ok = c.Notifications[n.Category]
		}
		if !ok {
			return ErrCategoryNotFound
		}
		n.Originator = "oauth"
	case permission.TypeWebapp:
		slug := strings.TrimPrefix(perm.SourceID, consts.Apps+"/")
		m, err := app.GetWebappBySlug(inst, slug)
		if err != nil {
			return err
		}
		notifications := m.Notifications()
		if notifications == nil {
			return ErrNoCategory
		}
		var ok bool
		p, ok = notifications[n.Category]
		if !ok {
			return ErrCategoryNotFound
		}
		n.Slug = m.Slug()
		n.Originator = "app"
	case permission.TypeKonnector:
		slug := strings.TrimPrefix(perm.SourceID, consts.Apps+"/")
		m, err := app.GetKonnectorBySlug(inst, slug)
		if err != nil {
			return err
		}
		notifications := m.Notifications()
		if notifications == nil {
			return ErrNoCategory
		}
		var ok bool
		p, ok = notifications[n.Category]
		if !ok {
			return ErrCategoryNotFound
		}
		n.Slug = m.Slug()
		n.Originator = "konnector"
	default:
		return ErrUnauthorized
	}

	return makePush(inst, &p, n)
}

func makePush(inst *instance.Instance, p *notification.Properties, n *notification.Notification) error {
	lastSent := time.Now()
	skipNotification := false

	// XXX: for retro-compatibility, we do not yet block applications from
	// sending notification from unknown category.
	if p != nil && p.Stateful {
		last, err := findLastNotification(inst, n.Source())
		if err != nil {
			return err
		}
		// when the state is the same for the last notification from this source,
		// we do not bother sending or creating a new notification.
		if last != nil {
			if last.State == n.State {
				inst.Logger().WithNamespace("notifications").
					Debugf("Notification %v was not sent (collapsed by same state %s)", p, n.State)
				return nil
			}
			if p.MinInterval > 0 && time.Until(last.LastSent) <= p.MinInterval {
				skipNotification = true
			}
		}

		if p.Stateful && !skipNotification {
			if b, ok := n.State.(bool); ok && !b {
				skipNotification = true
			} else if i, ok := n.State.(int); ok && i == 0 {
				skipNotification = true
			}
		}

		if skipNotification && last != nil {
			lastSent = last.LastSent
		}
	}

	preferredChannels := ensureMailFallback(n.PreferredChannels)
	at := n.At

	n.NID = ""
	n.NRev = ""
	n.SourceID = n.Source()
	n.CreatedAt = time.Now()
	n.LastSent = lastSent
	n.PreferredChannels = nil
	n.At = ""

	if err := couchdb.CreateDoc(inst, n); err != nil {
		return err
	}
	if skipNotification {
		return nil
	}

	var errm error
	log := inst.Logger().WithNamespace("notifications")
	for _, channel := range preferredChannels {
		switch channel {
		case "mobile":
			if p != nil {
				log.Infof("Sending push %#v: %v", p, n.State)
				err := sendPush(inst, p, n, at)
				if err == nil {
					return nil
				}
				log.Errorf("Error while sending push %#v: %v. Error: %v", p, n.State, err)
				errm = multierror.Append(errm, err)
			}
		case "mail":
			err := sendMail(inst, p, n, at)
			if err == nil {
				return nil
			}
			errm = multierror.Append(errm, err)
		case "sms":
			log.Infof("Sending SMS: %v", n.State)
			err := sendSMS(inst, p, n, at)
			if err == nil {
				return nil
			}
			log.Errorf("Error while sending sms: %s", err)
			errm = multierror.Append(errm, err)
		default:
			err := fmt.Errorf("Unknown channel for notification: %s", channel)
			errm = multierror.Append(errm, err)
		}
	}
	return errm
}

func findLastNotification(inst *instance.Instance, source string) (*notification.Notification, error) {
	var notifs []*notification.Notification
	req := &couchdb.FindRequest{
		UseIndex: "by-source-id",
		Selector: mango.Equal("source_id", source),
		Sort: mango.SortBy{
			{Field: "source_id", Direction: mango.Desc},
			{Field: "created_at", Direction: mango.Desc},
		},
		Limit: 1,
	}
	err := couchdb.FindDocs(inst, consts.Notifications, req, &notifs)
	if err != nil {
		return nil, err
	}
	if len(notifs) == 0 {
		return nil, nil
	}
	return notifs[0], nil
}

func sendPush(inst *instance.Instance,
	p *notification.Properties,
	n *notification.Notification,
	at string,
) error {
	if !hasNotifiableDevice(inst) {
		return errors.New("No device with push notification")
	}
	email := buildMailMessage(p, n)
	push := PushMessage{
		NotificationID: n.ID(),
		Source:         n.Source(),
		Title:          n.Title,
		Message:        n.Message,
		Priority:       n.Priority,
		Sound:          n.Sound,
		Data:           n.Data,
		Collapsible:    p.Collapsible,
		MailFallback:   email,
	}
	msg, err := job.NewMessage(&push)
	if err != nil {
		return err
	}
	return pushJobOrTrigger(inst, msg, "push", at)
}

func sendMail(inst *instance.Instance,
	p *notification.Properties,
	n *notification.Notification,
	at string,
) error {
	email := buildMailMessage(p, n)
	if email == nil {
		return nil
	}
	msg, err := job.NewMessage(&email)
	if err != nil {
		return err
	}
	return pushJobOrTrigger(inst, msg, "sendmail", at)
}

func sendSMS(inst *instance.Instance,
	p *notification.Properties,
	n *notification.Notification,
	at string,
) error {
	email := buildMailMessage(p, n)
	msg, err := job.NewMessage(&SMS{
		NotificationID: n.ID(),
		Message:        n.Message,
		MailFallback:   email,
	})
	if err != nil {
		return err
	}
	return pushJobOrTrigger(inst, msg, "sms", at)
}

func buildMailMessage(p *notification.Properties, n *notification.Notification) *mail.Options {
	email := mail.Options{Mode: mail.ModeFromStack}

	// Notifications from the stack have their own mail templates defined
	if p != nil && p.MailTemplate != "" {
		email.TemplateName = p.MailTemplate
		email.TemplateValues = n.Data
	} else if n.ContentHTML != "" {
		email.Subject = n.Title
		email.Parts = make([]*mail.Part, 0, 2)
		if n.Content != "" {
			email.Parts = append(email.Parts,
				&mail.Part{Body: n.Content, Type: "text/plain"})
		}
		if n.ContentHTML != "" {
			email.Parts = append(email.Parts,
				&mail.Part{Body: n.ContentHTML, Type: "text/html"})
		}
	} else {
		return nil
	}

	return &email
}

func pushJobOrTrigger(inst *instance.Instance, msg job.Message, worker, at string) error {
	if at == "" {
		_, err := job.System().PushJob(inst, &job.JobRequest{
			WorkerType: worker,
			Message:    msg,
		})
		return err
	}
	t, err := job.NewTrigger(inst, job.TriggerInfos{
		Type:       "@at",
		WorkerType: worker,
		Arguments:  at,
	}, msg)
	if err != nil {
		return err
	}
	return job.System().AddTrigger(t)
}

func ensureMailFallback(channels []string) []string {
	for _, c := range channels {
		if c == "mail" {
			return channels
		}
	}
	return append(channels, "mail")
}

func hasNotifiableDevice(inst *instance.Instance) bool {
	cs, err := oauth.GetNotifiables(inst)
	return err == nil && len(cs) > 0
}
