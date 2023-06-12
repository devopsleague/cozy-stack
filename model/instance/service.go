package instance

import (
	"encoding/json"
	"time"

	"github.com/cozy/cozy-stack/pkg/cache"
	"github.com/cozy/cozy-stack/pkg/couchdb"
	"github.com/cozy/cozy-stack/pkg/lock"
	"github.com/cozy/cozy-stack/pkg/logger"
	"github.com/cozy/cozy-stack/pkg/prefixer"
)

const cacheTTL = 5 * time.Minute
const cachePrefix = "i:"

type InstanceService struct {
	cache  cache.Cache
	logger *logger.Entry
}

func NewService(cache cache.Cache, lock lock.Getter) *InstanceService {
	return &InstanceService{
		cache:  cache,
		logger: logger.WithNamespace("instance"),
	}
}

// Get finds an instance from its domain by using CouchDB or the cache.
func (s *InstanceService) Get(domain string) (*Instance, error) {
	if data, ok := s.cache.Get(cachePrefix + domain); ok {
		inst := &Instance{}
		err := json.Unmarshal(data, inst)
		if err == nil && inst.MakeVFS() == nil {
			return inst, nil
		}
	}

	inst, err := s.GetFromCouch(domain)
	if err != nil {
		return nil, err
	}

	if data, err := json.Marshal(inst); err == nil {
		s.cache.SetNX(cacheKey(inst), data, cacheTTL)
	}
	return inst, nil
}

// GetFromCouch finds an instance in CouchDB from its domain.
//
// NOTE: You should probably use [InstanceService.Get] instead. This method
// is only useful if you want to bypass the cache.
func (s *InstanceService) GetFromCouch(domain string) (*Instance, error) {
	var res couchdb.ViewResponse
	err := couchdb.ExecView(prefixer.GlobalPrefixer, couchdb.DomainAndAliasesView, &couchdb.ViewRequest{
		Key:         domain,
		IncludeDocs: true,
		Limit:       1,
	}, &res)
	if couchdb.IsNoDatabaseError(err) {
		return nil, ErrNotFound
	}

	if err != nil {
		return nil, err
	}

	if len(res.Rows) == 0 {
		return nil, ErrNotFound
	}

	inst := &Instance{}
	err = json.Unmarshal(res.Rows[0].Doc, inst)
	if err != nil {
		return nil, err
	}

	if err = inst.MakeVFS(); err != nil {
		return nil, err
	}

	return inst, nil
}

// Update saves the changes in CouchDB.
func (s *InstanceService) Update(inst *Instance) error {
	if err := couchdb.UpdateDoc(prefixer.GlobalPrefixer, inst); err != nil {
		return err
	}

	if data, err := json.Marshal(inst); err == nil {
		s.cache.Set(inst.cacheKey(), data, cacheTTL)
	}

	return nil
}

func cacheKey(inst *Instance) string {
	return cachePrefix + inst.Domain
}
