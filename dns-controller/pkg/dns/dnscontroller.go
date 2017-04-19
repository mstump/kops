/*
Copyright 2016 The Kubernetes Authors.

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

package dns

import (
	"fmt"
	"time"

	"github.com/golang/glog"

	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"k8s.io/kops/dns-controller/pkg/util"
	"k8s.io/kubernetes/federation/pkg/dnsprovider"
	"k8s.io/kubernetes/federation/pkg/dnsprovider/rrstype"
)

var zoneListCacheValidity = time.Minute * 15

const DefaultTTL = time.Minute

// DNSController applies the desired DNS state to the DNS backend
type DNSController struct {
	zoneRules *ZoneRules

	util.Stoppable

	dnsCache *dnsCache

	// mutex protects the following mutable state
	mutex sync.Mutex
	// scopes is a map for each top-level grouping
	scopes map[string]*DNSControllerScope
	// lastSuccessSnapshot is the last snapshot we were able to apply to DNS
	// This lets us perform incremental updates to DNS.
	lastSuccessfulSnapshot *snapshot

	// changeCount is a change-counter, which helps us avoid computation when nothing has changed
	changeCount uint64
}

// DNSController is a Context
var _ Context = &DNSController{}

// scope is a group of record objects
type DNSControllerScope struct {
	// ScopeName is the string id for this scope
	ScopeName string

	parent *DNSController

	// mutex protected the following mutable state
	mutex sync.Mutex

	// Ready is set if the populating controller has performed an initial synchronization of records
	Ready bool

	// Records is the map of actual records for this scope
	Records map[string][]Record
}

// DNSControllerScope is a Scope
var _ Scope = &DNSControllerScope{}

// NewDnsController creates a DnsController
func NewDNSController(dnsProvider dnsprovider.Interface, zoneRules *ZoneRules) (*DNSController, error) {
	dnsCache, err := newDNSCache(dnsProvider)
	if err != nil {
		return nil, fmt.Errorf("error initializing DNS cache: %v", err)
	}

	c := &DNSController{
		scopes:    make(map[string]*DNSControllerScope),
		zoneRules: zoneRules,
		dnsCache:  dnsCache,
	}

	return c, nil
}

// Run starts the DnsController.
func (c *DNSController) Run() {
	glog.Infof("starting DNS controller")

	stopCh := c.StopChannel()
	go c.runWatcher(stopCh)

	<-stopCh
	glog.Infof("shutting down DNS controller")
}

func (c *DNSController) runWatcher(stopCh <-chan struct{}) {
	for {
		err := c.runOnce()
		if c.StopRequested() {
			glog.Infof("exiting dns controller loop")
			return
		}

		if err != nil {
			glog.Warningf("Unexpected error in DNS controller, will retry: %v", err)
			time.Sleep(10 * time.Second)
		} else {
			// Simple debouncing; DNS servers are typically pretty slow anyway
			time.Sleep(5 * time.Second)
		}
	}
}

type snapshot struct {
	changeCount  uint64
	records      []Record
	aliasTargets map[string][]Record

	recordValues map[recordKey][]string
}

func (c *DNSController) snapshotIfChangedAndReady() *snapshot {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	s := &snapshot{
		changeCount: atomic.LoadUint64(&c.changeCount),
	}

	aliasTargets := make(map[string][]Record)

	if c.lastSuccessfulSnapshot != nil && s.changeCount == c.lastSuccessfulSnapshot.changeCount {
		glog.V(6).Infof("No changes since DNS values last successfully applied")
		return nil
	}

	recordCount := 0
	for _, scope := range c.scopes {
		if !scope.Ready {
			glog.Infof("scope not yet ready: %s", scope.ScopeName)
			return nil
		}
		for _, scopeRecords := range scope.Records {
			recordCount += len(scopeRecords)
		}
	}

	records := make([]Record, 0, recordCount)
	for _, scope := range c.scopes {
		for _, scopeRecords := range scope.Records {
			for i := range scopeRecords {
				r := &scopeRecords[i]
				if r.AliasTarget {
					aliasTargets[r.FQDN] = append(aliasTargets[r.FQDN], *r)
				} else {
					records = append(records, *r)
				}
			}
		}
	}

	s.records = records
	s.aliasTargets = aliasTargets

	return s
}

type recordKey struct {
	RecordType RecordType
	FQDN       string
}

func (c *DNSController) runOnce() error {
	snapshot := c.snapshotIfChangedAndReady()
	if snapshot == nil {
		// Unchanged / not ready
		return nil
	}

	newValueMap := make(map[recordKey][]string)
	{
		// Resolve and build map
		for _, r := range snapshot.records {
			if r.RecordType == RecordTypeAlias {
				aliasRecords := snapshot.aliasTargets[r.Value]
				if len(aliasRecords) == 0 {
					glog.Infof("Alias in record specified %q, but no records were found for that name", r.Value)
				}
				for _, aliasRecord := range aliasRecords {
					key := recordKey{
						RecordType: aliasRecord.RecordType,
						FQDN:       r.FQDN,
					}
					// TODO: Support chains: alias of alias (etc)
					newValueMap[key] = append(newValueMap[key], aliasRecord.Value)
				}
				continue
			} else {
				key := recordKey{
					RecordType: r.RecordType,
					FQDN:       r.FQDN,
				}
				newValueMap[key] = append(newValueMap[key], r.Value)
				continue
			}
		}

		// Normalize
		for k, values := range newValueMap {
			sort.Strings(values)
			newValueMap[k] = values
		}
		snapshot.recordValues = newValueMap
	}

	var oldValueMap map[recordKey][]string
	if c.lastSuccessfulSnapshot != nil {
		oldValueMap = c.lastSuccessfulSnapshot.recordValues
	}

	op, err := newDNSOp(c.zoneRules, c.dnsCache)
	if err != nil {
		return err
	}

	// Store a list of all the errors, so that one bad apple doesn't block every other request
	var errors []error

	// Check each hostname for changes and apply them
	for k, newValues := range newValueMap {
		if c.StopRequested() {
			return fmt.Errorf("stop requested")
		}
		oldValues := oldValueMap[k]

		if util.StringSlicesEqual(newValues, oldValues) {
			glog.V(4).Infof("no change to records for %s", k)
			continue
		}

		ttl := DefaultTTL
		glog.Infof("Using default TTL of %v", ttl)

		glog.V(4).Infof("updating records for %s: %v -> %v", k, oldValues, newValues)

		// Duplicate records are a hard-error on e.g. Route53
		var dedup []string
		for _, s := range newValues {
			alreadyExists := false
			for _, e := range dedup {
				if e == s {
					alreadyExists = true
					break
				}
			}
			if alreadyExists {
				glog.V(2).Infof("skipping duplicate record %s", s)
				continue
			}
			dedup = append(dedup, s)
		}

		err := op.updateRecords(k, dedup, int64(ttl.Seconds()))
		if err != nil {
			glog.Infof("error updating records for %s: %v", k, err)
			errors = append(errors, err)
		}
	}

	// Look for deleted hostnames
	for k := range oldValueMap {
		if c.StopRequested() {
			return fmt.Errorf("stop requested")
		}

		newValues := newValueMap[k]
		if newValues == nil {
			err := op.deleteRecords(k)
			if err != nil {
				glog.Infof("error deleting records for %s: %v", k, err)
				errors = append(errors, err)
			}
		}
	}

	for key, changeset := range op.changesets {
		glog.V(2).Infof("applying DNS changeset for zone %s", key)
		if err := changeset.Apply(); err != nil {
			glog.Warningf("error applying DNS changeset for zone %s: %v", key, err)
			errors = append(errors, fmt.Errorf("error applying DNS changeset for zone %s: %v", key, err))
		}
	}

	if len(errors) != 0 {
		return errors[0]
	}

	// Success!  Store the snapshot as our new baseline
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.lastSuccessfulSnapshot = snapshot
	return nil
}

// dnsOp manages a single dns change; we cache results and state for the duration of the operation
type dnsOp struct {
	dnsCache     *dnsCache
	zones        map[string]dnsprovider.Zone
	recordsCache map[string][]dnsprovider.ResourceRecordSet

	changesets map[string]dnsprovider.ResourceRecordChangeset
}

func newDNSOp(zoneRules *ZoneRules, dnsCache *dnsCache) (*dnsOp, error) {
	zones, err := dnsCache.ListZones(zoneListCacheValidity)
	if err != nil {
		return nil, fmt.Errorf("error querying for zones: %v", err)
	}

	// First we build up a map of all zones by name,
	// then we go through and pick the "correct" zone for each name
	allZoneMap := make(map[string][]dnsprovider.Zone)
	for _, zone := range zones {
		name := EnsureDotSuffix(zone.Name())
		allZoneMap[name] = append(allZoneMap[name], zone)
	}

	zoneMap := make(map[string]dnsprovider.Zone)
	for name, zones := range allZoneMap {
		var matches []dnsprovider.Zone
		for _, zone := range zones {
			if zoneRules.MatchesExplicitly(zone) {
				matches = append(matches, zone)
			}
		}

		if len(matches) == 0 && zoneRules.Wildcard {
			// No explicit matches but wildcard; treat everything as matching
			matches = append(matches, zones...)
		}

		if len(matches) == 1 {
			zoneMap[name] = matches[0]
		} else if len(matches) > 1 {
			glog.Warningf("Found multiple zones for name %q, won't manage zone (To fix: provide zone mapping flag with ID of zone)", name)
		}
	}

	o := &dnsOp{
		dnsCache:     dnsCache,
		zones:        zoneMap,
		changesets:   make(map[string]dnsprovider.ResourceRecordChangeset),
		recordsCache: make(map[string][]dnsprovider.ResourceRecordSet),
	}

	return o, nil
}

func EnsureDotSuffix(s string) string {
	if !strings.HasSuffix(s, ".") {
		s = s + "."
	}
	return s
}

func (o *dnsOp) findZone(fqdn string) dnsprovider.Zone {
	zoneName := EnsureDotSuffix(fqdn)
	for {
		zone := o.zones[zoneName]
		if zone != nil {
			return zone
		}
		dot := strings.IndexByte(zoneName, '.')
		if dot == -1 {
			return nil
		}
		zoneName = zoneName[dot+1:]
	}
}

func (o *dnsOp) getChangeset(zone dnsprovider.Zone) (dnsprovider.ResourceRecordChangeset, error) {
	key := zone.Name() + "::" + zone.ID()
	changeset := o.changesets[key]
	if changeset == nil {
		rrsProvider, ok := zone.ResourceRecordSets()
		if !ok {
			return nil, fmt.Errorf("zone does not support resource records %q", zone.Name())
		}
		changeset = rrsProvider.StartChangeset()
		o.changesets[key] = changeset
	}

	return changeset, nil
}

// listRecords is a wrapper around listing records, but will cache the results for the duration of the dnsOp
func (o *dnsOp) listRecords(zone dnsprovider.Zone) ([]dnsprovider.ResourceRecordSet, error) {
	key := zone.Name() + "::" + zone.ID()

	rrs := o.recordsCache[key]
	if rrs == nil {
		rrsProvider, ok := zone.ResourceRecordSets()
		if !ok {
			return nil, fmt.Errorf("zone does not support resource records %q", zone.Name())
		}

		glog.V(2).Infof("Querying all dnsprovider records for zone %q", zone.Name())
		var err error
		rrs, err = rrsProvider.List()
		if err != nil {
			return nil, fmt.Errorf("error querying resource records for zone %q: %v", zone.Name(), err)
		}

		o.recordsCache[key] = rrs
	}

	return rrs, nil
}

func (o *dnsOp) deleteRecords(k recordKey) error {
	glog.V(2).Infof("Deleting all records for %s", k)

	fqdn := EnsureDotSuffix(k.FQDN)

	zone := o.findZone(fqdn)
	if zone == nil {
		// TODO: Post event into service / pod
		return fmt.Errorf("no suitable zone found for %q", fqdn)
	}

	rrs, err := o.listRecords(zone)
	if err != nil {
		return fmt.Errorf("error querying resource records for zone %q: %v", zone.Name(), err)
	}

	cs, err := o.getChangeset(zone)
	if err != nil {
		return err
	}

	for _, rr := range rrs {
		rrName := EnsureDotSuffix(rr.Name())
		if rrName != fqdn {
			glog.V(8).Infof("Skipping delete of record %q (name != %s)", rrName, fqdn)
			continue
		}
		if string(rr.Type()) != string(k.RecordType) {
			glog.V(8).Infof("Skipping delete of record %q (type %s != %s)", rrName, rr.Type(), k.RecordType)
			continue
		}

		glog.V(2).Infof("Deleting resource record %s %s", rrName, rr.Type())
		cs.Remove(rr)
	}

	return nil
}

func (o *dnsOp) updateRecords(k recordKey, newRecords []string, ttl int64) error {
	fqdn := EnsureDotSuffix(k.FQDN)

	zone := o.findZone(fqdn)
	if zone == nil {
		// TODO: Post event into service / pod
		return fmt.Errorf("no suitable zone found for %q", fqdn)
	}

	rrsProvider, ok := zone.ResourceRecordSets()
	if !ok {
		return fmt.Errorf("zone does not support resource records %q", zone.Name())
	}

	rrs, err := o.listRecords(zone)
	if err != nil {
		return fmt.Errorf("error querying resource records for zone %q: %v", zone.Name(), err)
	}

	var existing dnsprovider.ResourceRecordSet
	for _, rr := range rrs {
		rrName := EnsureDotSuffix(rr.Name())
		if rrName != fqdn {
			glog.V(8).Infof("Skipping record %q (name != %s)", rrName, fqdn)
			continue
		}
		if string(rr.Type()) != string(k.RecordType) {
			glog.V(8).Infof("Skipping record %q (type %s != %s)", rrName, rr.Type(), k.RecordType)
			continue
		}

		if existing != nil {
			glog.Warningf("Found multiple matching records: %v and %v", existing, rr)
		} else {
			glog.V(8).Infof("Found matching record: %s %s", k.RecordType, rrName)
		}
		existing = rr
	}

	cs, err := o.getChangeset(zone)
	if err != nil {
		return err
	}

	if existing != nil {
		glog.V(2).Infof("will replace existing dns record %s %s", existing.Type(), existing.Name())
		cs.Remove(existing)
	}

	glog.V(2).Infof("Adding DNS changes to batch %s %s", k, newRecords)
	rr := rrsProvider.New(fqdn, newRecords, ttl, rrstype.RrsType(k.RecordType))
	cs.Add(rr)

	return nil
}

func (c *DNSController) recordChange() {
	atomic.AddUint64(&c.changeCount, 1)
}

func (s *DNSControllerScope) MarkReady() {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.Ready = true
}

func (s *DNSControllerScope) Replace(recordName string, records []Record) {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	existing, exists := s.Records[recordName]

	if len(records) == 0 {
		if !exists {
			glog.V(6).Infof("skipping spurious removal of record %s/%s", s.ScopeName, recordName)
			return
		}

		delete(s.Records, recordName)
	} else {
		if recordsSliceEquals(existing, records) {
			glog.V(6).Infof("skipping spurious update of record %s/%s=%s", s.ScopeName, recordName, records)
			return
		}

		s.Records[recordName] = records
	}

	glog.V(2).Infof("Update desired state: %s/%s: %v", s.ScopeName, recordName, records)
	s.parent.recordChange()
}

// recordsSliceEquals compares two []Record
func recordsSliceEquals(l, r []Record) bool {
	if len(l) != len(r) {
		return false
	}
	for i := range l {
		if l[i] != r[i] {
			return false
		}
	}
	return true
}

// CreateScope creates a scope object.
func (c *DNSController) CreateScope(scopeName string) (Scope, error) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	s := c.scopes[scopeName]
	if s != nil {
		// We can't support this then we would need to change Ready to a counter
		// (OK, so we could, but it's probably an error anyway)
		return nil, fmt.Errorf("duplicate scope: %q", scopeName)
	}

	s = &DNSControllerScope{
		ScopeName: scopeName,
		Records:   make(map[string][]Record),
		parent:    c,
		Ready:     false,
	}
	c.scopes[scopeName] = s
	return s, nil
}
