/*
 * Copyright 2019-2020 VMware, Inc.
 * All Rights Reserved.
* Licensed under the Apache License, Version 2.0 (the "License");
* you may not use this file except in compliance with the License.
* You may obtain a copy of the License at
*   http://www.apache.org/licenses/LICENSE-2.0
* Unless required by applicable law or agreed to in writing, software
* distributed under the License is distributed on an "AS IS" BASIS,
* WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
* See the License for the specific language governing permissions and
* limitations under the License.
*/

package gslbutils

import (
	"errors"
	"strconv"
	"sync"

	gdpv1alpha1 "github.com/avinetworks/amko/internal/apis/amko/v1alpha1"

	"github.com/vmware/load-balancer-and-ingress-services-for-kubernetes/pkg/utils"
)

type GDPObj struct {
	Namespace string
	Name      string
	GDPLock   sync.RWMutex
}

var gdpObj GDPObj

func SetGDPObj(name, ns string) {
	gdpObj.GDPLock.Lock()
	defer gdpObj.GDPLock.Unlock()
	gdpObj.Name = name
	gdpObj.Namespace = ns
}

func GetGDPObj() (string, string) {
	gdpObj.GDPLock.RLock()
	defer gdpObj.GDPLock.RUnlock()
	return gdpObj.Name, gdpObj.Namespace
}

func IsEmpty() bool {
	gdpObj.GDPLock.RLock()
	defer gdpObj.GDPLock.RUnlock()

	if gdpObj.Name == "" && gdpObj.Namespace == "" {
		return true
	}
	return false
}

var (
	// Need to keep this global since, it will be used across multiple layers and multiple handlers
	Gfi    *GlobalFilter
	gfOnce sync.Once
)

// GlobalFilter is all the filters at one place. It also holds a list of ApplicableClusters
// to which all the filters are applicable. This list cannot be empty.
type GlobalFilter struct {
	// AppFilter contains rules for selecting applications
	AppFilter *AppFilter
	// NamespaceRules contains NamespaceSelector rules
	NSFilter *NamespaceFilter
	// TrafficSplit provides weights of traffic routed to different clusters
	TrafficSplit []ClusterTraffic
	// ApplicableClusters contain the list of clusters on which the filters
	// will be applicable
	ApplicableClusters []string
	Checksum           uint32
	// Respective filters for the namespaces.
	// NSFilterMap map[string]*NSFilter
	// GlobalLock is locked before accessing any of the filters.
	GlobalLock sync.RWMutex
}

// GetGlobalFilter returns the existing global filter
func GetGlobalFilter() *GlobalFilter {
	gfOnce.Do(func() {
		Gfi = GetNewGlobalFilter()
	})
	return Gfi
}

func (gf *GlobalFilter) GetNSFilterLabel() (Label, error) {
	gf.GlobalLock.RLock()
	defer gf.GlobalLock.RUnlock()

	if gf.NSFilter == nil {
		return Label{}, errors.New("no NSFilter present")
	}

	return gf.NSFilter.GetFilterLabel(), nil
}

func (gf *GlobalFilter) GetAppFilterLabel() (Label, error) {
	gf.GlobalLock.RLock()
	defer gf.GlobalLock.RUnlock()

	if gf.AppFilter == nil {
		return Label{}, errors.New("no appFilter present")
	}

	return gf.AppFilter.Label, nil
}

func (gf *GlobalFilter) IsClusterAllowed(cname string) bool {
	gf.GlobalLock.RLock()
	defer gf.GlobalLock.RUnlock()

	if PresentInList(cname, gf.ApplicableClusters) {
		return true
	}
	return false
}

func (gf *GlobalFilter) AddNSToNSFilter(cname, ns string) error {
	gf.GlobalLock.Lock()
	defer gf.GlobalLock.Unlock()

	if gf.NSFilter == nil {
		return errors.New("NSFilter empty in GlobalFilter, can't add namespace")
	}
	gf.NSFilter.AddNS(cname, ns)

	return nil
}

type AppFilter struct {
	Label
}

type NamespaceFilter struct {
	Label
	// SelectedNS contains a list of namespaces selected via this filter
	// updated by the namespace event handlers
	SelectedNS map[string][]string
	// Checksum to check for changes if GDP changes and to see if a
	// re-application of namespaces is required
	Checksum uint32
	Lock     sync.RWMutex
}

func (nsFilter *NamespaceFilter) GetChecksum() uint32 {
	nsFilter.Lock.RLock()
	defer nsFilter.Lock.RUnlock()
	return nsFilter.Checksum
}

func (nsFilter *NamespaceFilter) GetFilterLabel() Label {
	nsFilter.Lock.RLock()
	defer nsFilter.Lock.RUnlock()
	return nsFilter.Label
}

func (nsFilter *NamespaceFilter) AddNS(cname, ns string) {
	nsFilter.Lock.Lock()
	defer nsFilter.Lock.Unlock()

	nsList, ok := nsFilter.SelectedNS[cname]
	if !ok {
		nsFilter.SelectedNS[cname] = []string{ns}
		return
	}

	if !PresentInList(ns, nsList) {
		nsList = append(nsList, ns)
		nsFilter.SelectedNS[cname] = nsList
	}
}

type Label struct {
	Key   string
	Value string
}

func getLabelKeyAndValue(lbl map[string]string) (string, string) {
	for k, v := range lbl {
		return k, v
	}
	return "", ""
}

func createNewNSFilter(lbl map[string]string) *NamespaceFilter {
	k, v := getLabelKeyAndValue(lbl)
	nsFilter := NamespaceFilter{
		Label: Label{
			Key:   k,
			Value: v,
		},
	}
	// checksum for NSFilter only accounts for the key and label i.e., wrt
	// any GDP changes and not namespace changes
	cksum := utils.Hash(k + v)
	nsFilter.Checksum = cksum
	return &nsFilter
}

// AddToFilter handles creation of new filters, cluster or otherwise.
// Each namespace can have only one GDP object and one filter respectively, this is
// taken care of in the admission controller.
func (gf *GlobalFilter) AddToFilter(gdp *gdpv1alpha1.GlobalDeploymentPolicy) {
	gf.GlobalLock.Lock()
	defer gf.GlobalLock.Unlock()
	if len(gdp.Spec.MatchRules.AppSelector.Label) == 1 {
		k, v := getLabelKeyAndValue(gdp.Spec.MatchRules.AppSelector.Label)
		appFilter := AppFilter{
			Label: Label{
				Key:   k,
				Value: v,
			},
		}
		gf.AppFilter = &appFilter
	}
	if len(gdp.Spec.MatchRules.NamespaceSelector.Label) == 1 {
		gf.NSFilter = createNewNSFilter(gdp.Spec.MatchRules.NamespaceSelector.Label)
	}
	// Add applicable clusters
	gf.ApplicableClusters = gdp.Spec.MatchClusters
	// Add traffic split
	for _, ts := range gdp.Spec.TrafficSplit {
		ct := ClusterTraffic{
			ClusterName: ts.Cluster,
			Weight:      int32(ts.Weight),
		}
		gf.TrafficSplit = append(gf.TrafficSplit, ct)
	}
	gf.ComputeChecksum()
	Logf("ns: %s, object: NSFilter, msg: added/changed the global filter", gdp.ObjectMeta.Namespace)
}

func (gf *GlobalFilter) ComputeChecksum() {
	var cksum uint32

	if gf.AppFilter != nil {
		cksum += utils.Hash(gf.AppFilter.Key + gf.AppFilter.Value)
	}
	if gf.NSFilter != nil {
		cksum += gf.NSFilter.GetChecksum()
	}
	for _, c := range gf.ApplicableClusters {
		cksum += utils.Hash(c)
	}
	for _, ts := range gf.TrafficSplit {
		cksum += utils.Hash(ts.ClusterName + strconv.Itoa(int(ts.Weight)))
	}
	gf.Checksum = cksum
}

func (gf *GlobalFilter) GetTrafficWeight(ns, cname string) (int32, error) {
	gf.GlobalLock.RLock()
	defer gf.GlobalLock.RUnlock()
	for _, ts := range gf.TrafficSplit {
		if ts.ClusterName == cname {
			return ts.Weight, nil
		}
	}
	Logf("cname: %s, msg: no weight available for this cluster", cname)
	return 0, errors.New("no weight available for cluster " + cname)
}

func PresentInList(key string, strList []string) bool {
	for _, str := range strList {
		if str == key {
			return true
		}
	}
	return false
}

func isTrafficWeightChanged(new, old *gdpv1alpha1.GlobalDeploymentPolicy) bool {
	// There are 3 conditions when a cluster traffic ratio is different between the old
	// and new GDP objects:
	// 1. Length of the Traffic Split elements is different between the two.
	// 2. Length is same, but a member from the old list is not found in the new list.
	// 3. Length is same, but a member has different ratios across both the objects.

	if len(old.Spec.TrafficSplit) != len(new.Spec.TrafficSplit) {
		return true
	}
	for _, oldMember := range old.Spec.TrafficSplit {
		found := false
		for _, newMember := range new.Spec.TrafficSplit {
			if oldMember.Cluster == newMember.Cluster {
				found = true
				if oldMember.Weight != newMember.Weight {
					return true
				}
			}
		}
		if found == false {
			// this member was not found in the new GDP, so return true
			return true
		}
	}
	return false
}

// UpdateGlobalFilter takes two arguments: the old and the new GDP objects, and verifies
// whether a change is required to any of the filters. If yes, it changes either the cluster
// filter or one of the namespace filters.
func (gf *GlobalFilter) UpdateGlobalFilter(oldGDP, newGDP *gdpv1alpha1.GlobalDeploymentPolicy) (bool, bool) {
	// Need to check for the NSFilterMap
	nf := GetNewGlobalFilter()
	nf.AddToFilter(newGDP)

	Logf("ns: %s, gdp: %s, msg: %s", oldGDP.ObjectMeta.Namespace, oldGDP.ObjectMeta.Name,
		"got an update event")
	gf.GlobalLock.Lock()
	defer gf.GlobalLock.Unlock()
	Debugf("old checksum: %d, new checksum: %d", gf.Checksum, nf.Checksum)
	if gf.Checksum == nf.Checksum {
		// No updates needed, just return
		return false, false
	}
	Logf("ns: %s, gdp: %s, object: filter, msg: %s", oldGDP.ObjectMeta.Namespace, oldGDP.ObjectMeta.Name,
		"filter changed, will update filter and re-evaluate objects")
	// update the filter if the checksums changed
	gf.AppFilter = nf.AppFilter
	gf.NSFilter = nf.NSFilter
	gf.TrafficSplit = nf.TrafficSplit
	gf.ApplicableClusters = nf.ApplicableClusters
	gf.Checksum = nf.Checksum

	trafficWeightChanged := isTrafficWeightChanged(newGDP, oldGDP)
	return true, trafficWeightChanged
}

// DeleteFromGlobalFilter deletes a filter pertaining to gdp.
func (gf *GlobalFilter) DeleteFromGlobalFilter(gdp *gdpv1alpha1.GlobalDeploymentPolicy) {
	gf.GlobalLock.Lock()
	defer gf.GlobalLock.Unlock()
	gf.AppFilter = nil
	gf.NSFilter = nil
	gf.ApplicableClusters = []string{}
	gf.Checksum = 0
	gf.TrafficSplit = []ClusterTraffic{}
}

// GetNewGlobalFilter returns a new GlobalFilter. It is to be called only once with the
// the GDP object as the input. Either the namespace of the GDP object is AVISystem
// or its some other namespace. Based on that this GlobalFilter is created.
func GetNewGlobalFilter() *GlobalFilter {
	gf := &GlobalFilter{
		AppFilter:          nil,
		NSFilter:           nil,
		TrafficSplit:       []ClusterTraffic{},
		ApplicableClusters: []string{},
	}
	return gf
}

// ClusterTraffic determines the "Weight" of traffic routed to a cluster with name "ClusterName"
type ClusterTraffic struct {
	ClusterName string
	Weight      int32
}
