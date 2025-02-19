/*
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

package scheduling

import (
	"fmt"
	"strings"
	"sync/atomic"

	v1 "k8s.io/api/core/v1"

	"github.com/aws/karpenter/pkg/apis/provisioning/v1alpha5"
	"github.com/aws/karpenter/pkg/cloudprovider"
	"github.com/aws/karpenter/pkg/utils/resources"
)

// Node is a set of constraints, compatible pods, and possible instance types that could fulfill these constraints. This
// will be turned into one or more actual node instances within the cluster after bin packing.
type Node struct {
	Hostname            string
	Provisioner         *v1alpha5.Provisioner
	InstanceTypeOptions []cloudprovider.InstanceType
	Pods                []*v1.Pod

	topology *Topology
	requests v1.ResourceList
}

var nodeID int64

func NewNode(provisioner *v1alpha5.Provisioner, topology *Topology, daemonResources v1.ResourceList, instanceTypes []cloudprovider.InstanceType) *Node {
	n := &Node{
		Hostname:            fmt.Sprintf("hostname-placeholder-%04d", atomic.AddInt64(&nodeID, 1)),
		Provisioner:         provisioner.DeepCopy(),
		InstanceTypeOptions: instanceTypes,
		topology:            topology,
		requests:            daemonResources,
	}
	n.Provisioner.Spec.Requirements = n.Provisioner.Spec.Requirements.Add(v1.NodeSelectorRequirement{
		Key:      v1.LabelHostname,
		Operator: v1.NodeSelectorOpIn,
		Values:   []string{n.Hostname},
	})
	topology.Register(v1.LabelHostname, n.Hostname)
	return n
}

func (n *Node) Add(pod *v1.Pod) error {
	// Check tolerations
	if err := n.Provisioner.Spec.Taints.Tolerates(pod); err != nil {
		return err
	}

	podRequirements := v1alpha5.NewPodRequirements(pod)
	// Check initial compatibility
	if err := n.Provisioner.Spec.Requirements.Compatible(podRequirements); err != nil {
		return err
	}
	nodeRequirements := n.Provisioner.Spec.Requirements.Add(podRequirements.Requirements...)

	// Include topology requirements
	requirements, err := n.topology.AddRequirements(podRequirements, nodeRequirements, pod)
	if err != nil {
		return err
	}

	// Check node compatibility
	if err = n.Provisioner.Spec.Requirements.Compatible(requirements); err != nil {
		return err
	}

	// Tighten requirements
	requirements = n.Provisioner.Spec.Requirements.Add(requirements.Requirements...)
	requests := resources.Merge(n.requests, resources.RequestsForPods(pod))

	// Check instance type combinations
	instanceTypes := cloudprovider.FilterInstanceTypes(n.InstanceTypeOptions, requirements, requests)
	if len(instanceTypes) == 0 {
		return fmt.Errorf("no instance type satisfied resources %s and requirements %s", resources.String(resources.RequestsForPods(pod)), requirements)
	}
	// Update node
	n.Pods = append(n.Pods, pod)
	n.InstanceTypeOptions = instanceTypes
	n.requests = requests
	n.Provisioner.Spec.Requirements = requirements
	n.topology.Record(pod, requirements)
	return nil
}

func (n *Node) String() string {
	var itSb strings.Builder
	for i, it := range n.InstanceTypeOptions {
		// print the first 5 instance types only (indices 0-4)
		if i > 4 {
			fmt.Fprintf(&itSb, " and %d other(s)", len(n.InstanceTypeOptions)-i)
			break
		} else if i > 0 {
			fmt.Fprint(&itSb, ", ")
		}
		fmt.Fprint(&itSb, it.Name())
	}
	return fmt.Sprintf("node with %d pods requesting %s from types %s", len(n.Pods), resources.String(n.requests), itSb.String())
}
