package records

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"

	"errors"

	"strconv"

	"bosh-dns/dns/server/aliases"
	"bosh-dns/dns/server/healthiness"
	"bosh-dns/dns/server/records/internal"

	boshlog "github.com/cloudfoundry/bosh-utils/logger"
	"github.com/miekg/dns"
)

type recordGroup map[*Record]struct{}

type RecordSet struct {
	recordFileReader  FileReader
	recordsMutex      sync.RWMutex
	subscriberssMutex sync.RWMutex
	subscribers       []chan bool
	logger            boshlog.Logger
	aliasList         aliases.Config
	healthWatcher     healthiness.HealthWatcher

	trackedDomains *internal.PriorityLimitedTranscript

	trackedIPs      map[string]map[string]struct{}
	trackedIPsMutex *sync.Mutex

	domains []string
	Records []Record
}

func NewRecordSet(
	recordFileReader FileReader,
	aliasList aliases.Config,
	healthWatcher healthiness.HealthWatcher,
	maximumTrackedDomains uint,
	shutdownChan chan struct{},
	logger boshlog.Logger,
) (*RecordSet, error) {
	r := &RecordSet{
		recordFileReader: recordFileReader,
		logger:           logger,
		aliasList:        aliasList,
		healthWatcher:    healthWatcher,
		trackedDomains:   internal.NewPriorityLimitedTranscript(maximumTrackedDomains),
		trackedIPs:       map[string]map[string]struct{}{},
		trackedIPsMutex:  &sync.Mutex{},
	}

	r.update()

	go func() {
		subscriptionChan := recordFileReader.Subscribe()
		defer func() {
			r.subscriberssMutex.RLock()
			for _, subscriber := range r.subscribers {
				close(subscriber)
			}
			r.subscriberssMutex.RUnlock()
		}()

		for {
			select {
			case <-shutdownChan:
				return
			case ok := <-subscriptionChan:
				if !ok {
					return
				}

				r.update()
				r.refreshTrackedIPs()

				r.subscriberssMutex.RLock()
				for _, subscriber := range r.subscribers {
					subscriber <- true
				}
				r.subscriberssMutex.RUnlock()
			}
		}
	}()

	return r, nil
}

func (r *RecordSet) Subscribe() <-chan bool {
	r.subscriberssMutex.Lock()
	defer r.subscriberssMutex.Unlock()
	c := make(chan bool)
	r.subscribers = append(r.subscribers, c)
	return c
}

func (r *RecordSet) Resolve(fqdn string) ([]string, error) {
	r.recordsMutex.RLock()
	defer r.recordsMutex.RUnlock()

	if removed := r.trackedDomains.Touch(fqdn); removed != "" {
		r.untrackDomain(removed)
	}

	var (
		finalIPs []string
		errs     []error
	)

	resolutions := r.aliasList.Resolutions(fqdn)
	if len(resolutions) > 0 {
		for _, resolution := range resolutions {

			if net.ParseIP(resolution) != nil {
				finalIPs = append(finalIPs, resolution)
				continue
			}

			hostIPs, crit, err := r.resolveQuery(resolution)
			if err != nil {
				errs = append(errs, err)
			}

			healthyIPs, unhealthyIPs := r.segregateIPs(hostIPs, resolution)
			results := filterByHealthStrategy(healthyIPs, unhealthyIPs, crit)
			finalIPs = append(finalIPs, results...)
		}

		if len(finalIPs) == 0 && len(errs) > 0 {
			return nil, fmt.Errorf("failures occurred when resolving alias domains: %s", errs)
		}
	} else {
		if net.ParseIP(fqdn) != nil {
			finalIPs = []string{fqdn}
		} else {
			ips, crit, err := r.resolveQuery(fqdn)
			if err != nil {
				return nil, err
			}

			healthyIPs, unhealthyIPs := r.segregateIPs(ips, fqdn)
			finalIPs = filterByHealthStrategy(healthyIPs, unhealthyIPs, crit)
		}
	}

	return finalIPs, nil
}

func filterByHealthStrategy(healthyIPs, unhealthyIPs []string, crit criteria) []string {
	healthStrategy := "0"
	if len(crit["s"]) > 0 {
		healthStrategy = crit["s"][0]
	}

	switch healthStrategy {
	case "1": // unhealthy ones
		return unhealthyIPs
	case "3": // healthy
		return healthyIPs
	case "4": // all
		return append(healthyIPs, unhealthyIPs...)
	default: // smart strategy
		if len(healthyIPs) == 0 {
			return unhealthyIPs
		}

		return healthyIPs
	}
}

func (r *RecordSet) segregateIPs(ips []string, fqdn string) ([]string, []string) {
	var healthyIPs, unhealthyIPs []string
	for _, ip := range ips {
		r.trackedIPsMutex.Lock()
		r.trackedIPs[ip] = map[string]struct{}{}
		if _, ok := r.trackedIPs[ip]; !ok {
			r.trackedIPs[ip] = map[string]struct{}{}
		}
		r.trackedIPs[ip][fqdn] = struct{}{}
		r.trackedIPsMutex.Unlock()

		if r.healthWatcher.IsHealthy(ip) {
			healthyIPs = append(healthyIPs, ip)
		} else {
			unhealthyIPs = append(unhealthyIPs, ip)
		}
	}

	return healthyIPs, unhealthyIPs
}

func (r *RecordSet) refreshTrackedIPs() {
	newTrackedIPs := map[string]map[string]struct{}{}
	r.trackedIPsMutex.Lock()
	defer r.trackedIPsMutex.Unlock()
	for _, domain := range r.trackedDomains.Registry() {
		ips, _, err := r.resolveQuery(domain)
		if err != nil {
			continue
		}

		for _, ip := range ips {
			if _, ok := newTrackedIPs[ip]; !ok {
				newTrackedIPs[ip] = map[string]struct{}{}
			}
			newTrackedIPs[ip][domain] = struct{}{}

			if _, found := r.trackedIPs[ip]; found {
				delete(r.trackedIPs, ip)
			} else {
				r.healthWatcher.IsHealthy(ip)
			}
		}
	}

	for oldIP := range r.trackedIPs {
		r.healthWatcher.Untrack(oldIP)
	}

	r.trackedIPs = newTrackedIPs
}

func (r *RecordSet) untrackDomain(removedDomain string) {
	r.trackedIPsMutex.Lock()
	defer r.trackedIPsMutex.Unlock()

	for ip, domains := range r.trackedIPs {
		if _, ok := domains[removedDomain]; ok {
			delete(domains, removedDomain)
			if len(domains) == 0 {
				r.healthWatcher.Untrack(ip)
			}
		}
	}
}

func (r *RecordSet) Domains() []string {
	r.recordsMutex.RLock()
	defer r.recordsMutex.RUnlock()

	return append(r.domains, r.aliasList.AliasHosts()...)
}

func (r *RecordSet) update() {
	contents, err := r.recordFileReader.Get()
	if err != nil {
		return
	}
	records, err := createFromJSON(contents, r.logger)
	if err != nil {
		return
	}

	r.recordsMutex.Lock()
	defer r.recordsMutex.Unlock()

	r.Records = records

	domains := make(map[string]struct{})
	for _, record := range r.Records {
		domains[record.Domain] = struct{}{}
	}
	for domain := range domains {
		r.domains = append(r.domains, domain)
	}
}

func (r *RecordSet) ipsMatching(matcher Matcher) []string {
	ips := []string{}

	for _, record := range r.Records {
		if matcher.Match(&record) {
			ips = append(ips, record.IP)
		}
	}

	return ips
}

func (r *RecordSet) resolveQuery(fqdn string) ([]string, criteria, error) {
	var ips []string

	segments := strings.SplitN(fqdn, ".", 2) // [q-s0, q-g7.x.y.bosh]

	if len(segments) < 2 {
		return ips, criteria{}, errors.New("domain is malformed")
	}

	var tld string
	for _, possible := range r.domains { // do these/do these have to end in a . ?
		if strings.HasSuffix(fqdn, possible) {
			tld = possible
			break
		}
	}

	if tld == "" {
		return []string{}, criteria{}, nil
	}

	groupQuery := strings.TrimSuffix(segments[1], "."+tld)
	groupSegments := strings.Split(groupQuery, ".")
	var c criteria
	var err error
	if len(groupSegments) == 1 {
		c, err = parseCriteria(segments[0], groupQuery, "", "", "", tld)
		if err != nil {
			return ips, c, err
		}
	} else if len(groupSegments) == 3 {
		c, err = parseCriteria(segments[0], "", groupSegments[0], groupSegments[1], groupSegments[2], tld)
		if err != nil {
			return ips, c, err
		}
	} else {
		panic(fmt.Sprintf("Bad group segment query had %d values %#v\n", len(groupSegments), groupSegments))
	}

	matcher := new(AndMatcher)
	for field, values := range c {
		// healthiness is not handled by the normal recordset
		if field == "s" {
			continue
		}
		matcher.Append(Field(field, values))
	}

	return r.ipsMatching(matcher), c, nil
}

func createFromJSON(j []byte, logger boshlog.Logger) ([]Record, error) {
	swap := struct {
		Keys  []string        `json:"record_keys"`
		Infos [][]interface{} `json:"record_infos"`
	}{}

	err := json.Unmarshal(j, &swap)
	if err != nil {
		return nil, err
	}

	records := make([]Record, 0, len(swap.Infos))

	idIndex := -1
	numIDIndex := -1
	groupIndex := -1
	networkIndex := -1
	networkIDIndex := -1
	deploymentIndex := -1
	ipIndex := -1
	domainIndex := -1
	azIDIndex := -1
	instanceIndexIndex := -1
	groupIdsIndex := -1

	for i, k := range swap.Keys {
		switch k {
		case "id":
			idIndex = i
		case "num_id":
			numIDIndex = i
		case "instance_group":
			groupIndex = i
		case "group_ids":
			groupIdsIndex = i
		case "network":
			networkIndex = i
		case "network_id":
			networkIDIndex = i
		case "deployment":
			deploymentIndex = i
		case "ip":
			ipIndex = i
		case "domain":
			domainIndex = i
		case "az_id":
			azIDIndex = i
		case "instance_index":
			instanceIndexIndex = i
		default:
			continue
		}
	}

	countKeys := len(swap.Keys)

	for index, info := range swap.Infos {
		countInfo := len(info)
		if countInfo != countKeys {
			logger.Warn("RecordSet", "Unbalanced records structure. Found %d fields of an expected %d at record #%d", countInfo, countKeys, index)
			continue
		}

		var domainIndexStr string
		if !requiredStringValue(&domainIndexStr, info, domainIndex, "domain", index, logger) {
			continue
		}

		domain := dns.Fqdn(domainIndexStr)

		record := Record{Domain: domain}

		if !requiredStringValue(&record.ID, info, idIndex, "id", index, logger) {
			continue
		} else if !requiredStringValue(&record.Group, info, groupIndex, "group", index, logger) {
			continue
		} else if !requiredStringValue(&record.Network, info, networkIndex, "network", index, logger) {
			continue
		} else if !requiredStringValue(&record.Deployment, info, deploymentIndex, "deployment", index, logger) {
			continue
		} else if !requiredStringValue(&record.IP, info, ipIndex, "ip", index, logger) {
			continue
		} else if !optionalStringValue(&record.AZID, info, azIDIndex, "az_id", index, logger) {
			continue
		} else if !optionalStringValue(&record.NetworkID, info, networkIDIndex, "network_id", index, logger) {
			continue
		} else if !optionalStringValue(&record.NumId, info, numIDIndex, "num_id", index, logger) {
			continue
		} else if groupIdsIndex >= 0 && !assertStringArrayOfStringValue(&record.GroupIDs, info, groupIdsIndex, "group_ids", index, logger) {
			continue
		}

		assertStringIntegerValue(&record.InstanceIndex, info, instanceIndexIndex, "instance_index", index, logger)

		records = append(records, record)
	}

	return records, nil
}

func assertStringIntegerValue(field *string, info []interface{}, fieldIdx int, fieldName string, infoIdx int, logger boshlog.Logger) bool {
	if fieldIdx < 0 {
		return false
	}

	float64Value, ok := info[fieldIdx].(float64) // golang default type for numeric fields
	if !ok {
		logger.Warn("RecordSet", "Value %d (%s) of record %d is not expected type of %s: %#+v", fieldIdx, fieldName, infoIdx, "numeric", info[fieldIdx])
	}

	*field = strconv.Itoa(int(float64Value))
	return ok
}

func convertToStringValue(field *string, info []interface{}, fieldIdx int, fieldName string, infoIdx int, logger boshlog.Logger) bool {
	var ok bool
	*field, ok = info[fieldIdx].(string)

	if !ok {
		logger.Warn("RecordSet", "Value %d (%s) of record %d is not expected type of %s: %#+v", fieldIdx, fieldName, infoIdx, "string", info[fieldIdx])
	}

	return ok
}

func optionalStringValue(field *string, info []interface{}, fieldIdx int, fieldName string, infoIdx int, logger boshlog.Logger) bool {
	if fieldIdx >= 0 {
		if info[fieldIdx] == nil {
			info[fieldIdx] = ""
			return true
		}
		return convertToStringValue(field, info, fieldIdx, fieldName, infoIdx, logger)
	}

	return true
}

func requiredStringValue(field *string, info []interface{}, fieldIdx int, fieldName string, infoIdx int, logger boshlog.Logger) bool {
	if fieldIdx < 0 {
		return false
	}

	return convertToStringValue(field, info, fieldIdx, fieldName, infoIdx, logger)
}

func assertStringArrayOfStringValue(field *[]string, info []interface{}, fieldIdx int, fieldName string, infoIdx int, logger boshlog.Logger) bool {
	var ok bool
	var intermediateField []interface{}

	intermediateField, ok = info[fieldIdx].([]interface{})
	if !ok {
		logger.Warn("RecordSet", "Value %d (%s) of record %d is not expected type of %s: %#+v", fieldIdx, fieldName, infoIdx, "array of string", info[fieldIdx])
	}
	out := make([]string, len(intermediateField))
	for i, v := range intermediateField {
		out[i], ok = v.(string)
		if !ok {
			logger.Warn("RecordSet", "Value %d (%s) of record %d is not expected type of %s: %#+v", fieldIdx, fieldName, infoIdx, "array of string", info[fieldIdx])
			return ok
		}
	}

	*field = out

	return ok
}
