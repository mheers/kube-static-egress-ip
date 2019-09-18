/*
Copyright 2017 Nirmata inc.

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

package controller

import (
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/golang/glog"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"

	egressipAPI "github.com/nirmata/kube-static-egress-ip/pkg/apis/egressip/v1alpha1"
	clientset "github.com/nirmata/kube-static-egress-ip/pkg/client/clientset/versioned"
	informers "github.com/nirmata/kube-static-egress-ip/pkg/client/informers/externalversions/egressip/v1alpha1"
	listers "github.com/nirmata/kube-static-egress-ip/pkg/client/listers/egressip/v1alpha1"
	director "github.com/nirmata/kube-static-egress-ip/pkg/director"
	gateway "github.com/nirmata/kube-static-egress-ip/pkg/gateway"
	utils "github.com/nirmata/kube-static-egress-ip/pkg/utils"
	coreinformers "k8s.io/client-go/informers/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
)

const (
	controllerAgentName    = "egressip-controller"
	egressGatewayAnnotaion = "nirmata.io/staticegressips-gateway"
)

// Controller is the controller implementation for StaticEgressIP resources
type Controller struct {
	// kubeclientset is a standard kubernetes clientset
	kubeclientset kubernetes.Interface
	// egressIPclientset is a clientset for our own API group
	egressIPclientset clientset.Interface
	// egressIPLister can list/get StaticEgressIP from the shared informer's store
	egressIPLister listers.StaticEgressIPLister
	// egressIPsSynced returns true if the StaticEgressIP store has been synced at least once.
	egressIPsSynced cache.InformerSynced
	endpointsLister corelisters.EndpointsLister
	trafficDirector *director.EgressDirector
	trafficGateway  *gateway.EgressGateway
	workqueue       workqueue.RateLimitingInterface
}

// NewEgressIPController returns a new NewEgressIPController
func NewEgressIPController(
	kubeclientset kubernetes.Interface,
	egressIPclientset clientset.Interface,
	endpointsInformer coreinformers.EndpointsInformer,
	egressIPInformer informers.StaticEgressIPInformer) *Controller {

	controller := &Controller{
		kubeclientset:     kubeclientset,
		egressIPclientset: egressIPclientset,
		egressIPLister:    egressIPInformer.Lister(),
		endpointsLister:   endpointsInformer.Lister(),
		egressIPsSynced:   egressIPInformer.Informer().HasSynced,
		workqueue:         workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "StaticEgressIPs"),
	}

	glog.Info("Setting up event handlers to handle add/delete/update events to StaticEgressIP resources")
	// Set up an event handler for when StaticEgressIP resources change
	egressIPInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    controller.addStaticEgressIP,
		UpdateFunc: controller.updateStaticEgressIP,
		DeleteFunc: controller.deleteStaticEgressIP,
	})

	return controller
}

// Run will set up the event handlers for types we are interested in, as well
// as syncing informer caches and starting workers. It will block until stopCh
// is closed, at which point it will shutdown the workqueue and wait for
// workers to finish processing their current work items.
func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	var err error

	// Start the informer factories to begin populating the informer caches
	glog.Info("Starting StaticEgressIP controller")

	c.trafficGateway, err = gateway.NewEgressGateway()
	if err != nil {
		glog.Fatalf("Failed to setup node to act as egress traffic gateway: " + err.Error())
	}
	err = c.trafficGateway.Setup()
	if err != nil {
		glog.Fatalf("Failed to setup node to act as egress traffic gateway: " + err.Error())
	}
	glog.Info("Configured node to act as a egress traffic gateway")

	c.trafficDirector, err = director.NewEgressDirector()
	if err != nil {
		glog.Fatalf("Failed to setup node to act as egress traffic director: " + err.Error())
	}
	err = c.trafficDirector.Setup()
	if err != nil {
		glog.Fatalf("Failed to setup node to act as egress traffic director: " + err.Error())
	}
	glog.Info("Configured node to act as a egress traffic director")

	// Wait for the caches to be synced before starting workers
	glog.Info("Waiting for informer caches to sync")
	if ok := cache.WaitForCacheSync(stopCh, c.egressIPsSynced); !ok {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	glog.Info("Starting workers")
	// Launch two workers to process StaticEgressIP resources
	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	glog.Info("Started workers")
	<-stopCh
	glog.Info("Shutting down workers")

	return nil
}

// runWorker is a long-running function that will continually call the
// processNextWorkItem function in order to read and process a message on the
// workqueue.
func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

// processNextWorkItem will read a single work item off the workqueue and
// attempt to process it, by calling the syncHandler.
func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	// We wrap this block in a func so we can defer c.workqueue.Done.
	err := func(obj interface{}) error {
		// We call Done here so the workqueue knows we have finished
		// processing this item. We also must remember to call Forget if we
		// do not want this work item being re-queued. For example, we do
		// not call Forget if a transient error occurs, instead the item is
		// put back on the workqueue and attempted again after a back-off
		// period.
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		// We expect strings to come off the workqueue. These are of the
		// form namespace/name. We do this as the delayed nature of the
		// workqueue means the items in the informer cache may actually be
		// more up to date that when the item was initially put onto the
		// workqueue.
		if key, ok = obj.(string); !ok {
			// As the item in the workqueue is actually invalid, we call
			// Forget here else we'd go into a loop of attempting to
			// process a work item that is invalid.
			c.workqueue.Forget(obj)
			runtime.HandleError(fmt.Errorf("expected string in workqueue but got %#v", obj))
			return nil
		}
		// Run the syncHandler, passing it the namespace/name string of the
		// StaticEgressIP resource to be synced.
		if err := c.syncHandler(key); err != nil {
			// Put the item back on the workqueue to handle any transient errors.
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		// Finally, if no error occurs we Forget this item so it does not
		// get queued again until another change happens.
		c.workqueue.Forget(obj)
		glog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		runtime.HandleError(err)
		return true
	}

	return true
}

// syncHandler compares the actual state with the desired, and attempts to
// converge the two. It then updates the Status block of the StaticEgressIP resource
// with the current status of the resource.
func (c *Controller) syncHandler(key string) error {
	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		runtime.HandleError(fmt.Errorf("invalid resource key: %s", key))
		return nil
	}

	// Get the StaticEgressIP resource with this namespace/name
	staticEgressIP, err := c.egressIPLister.StaticEgressIPs(namespace).Get(name)
	if err != nil {
		// The StaticEgressIP resource may no longer exist, in which case we stop processing
		if k8serrors.IsNotFound(err) {
			runtime.HandleError(fmt.Errorf("StaticEgressIP '%s' in work queue no longer exists", key))
			return nil
		}
		return err
	}

	glog.Info("Processing update to StaticEgressIP: " + key)
	if staticEgressIP.Status.GatewayNode == "" {
		glog.Info("Gateway for forStaticEgressIP: " + key + " is not set yet, so ignoring the update")
		return nil
	}

	var isGateway bool
	isGateway, err = c.isEgressGateway(c.kubeclientset, staticEgressIP)
	if err != nil {
		return errors.New("Failed to identify the gateway for static egress IP: " + key + " due to" + err.Error())

	}
	if isGateway {
		return c.doGatewyProcessing(c.kubeclientset, staticEgressIP)
	} else {
		return c.doDirectorProcessing(c.kubeclientset, staticEgressIP)
	}
	return nil
}

// isEgressGateway returns true if node is configured as egress traffic gateway
func (c *Controller) isEgressGateway(clientset kubernetes.Interface, staticEgressIP *egressipAPI.StaticEgressIP) (bool, error) {
	nodeObject, err := utils.GetNodeObject(clientset, "")
	if err != nil {
		return false, err
	}
	return staticEgressIP.Status.GatewayNode == string(nodeObject.UID), nil
}

func (c *Controller) doDirectorProcessing(clientset kubernetes.Interface, staticEgressIP *egressipAPI.StaticEgressIP) error {
	if staticEgressIP.Status.GatewayIP == "" {
		glog.Info("Gateway IP for forStaticEgressIP: " + staticEgressIP.Name + " is not set yet, so ignoring the update")
		return nil
	}
	for i, rule := range staticEgressIP.Spec.Rules {
		endpoint, err := c.endpointsLister.Endpoints(staticEgressIP.Namespace).Get(rule.ServiceName)
		if err != nil {
			glog.Errorf("Failed to get endpoints object for service '%s' due to: '%s'", rule.ServiceName, err.Error())
		}
		ips := make([]string, 0)
		for _, epSubset := range endpoint.Subsets {
			for _, _ = range epSubset.Ports {
				for _, addr := range epSubset.Addresses {
					ips = append(ips, addr.IP)
				}
			}
		}
		err = c.trafficDirector.AddRouteToGateway(generateRuleId(staticEgressIP.Namespace, staticEgressIP.Name, i), ips, rule.Cidr, staticEgressIP.Status.GatewayIP)
		if err != nil {
			glog.Errorf("Failed to setup routes to send the egress traffic to gateway due to %s", err.Error())
		}
	}

	return nil
}

func (c *Controller) doGatewyProcessing(clientset kubernetes.Interface, staticEgressIP *egressipAPI.StaticEgressIP) error {

	gatewayIP, err := utils.GetGatewayIP()
	if err != nil {
		return err
	}
	copyObj := staticEgressIP.DeepCopy()
	copyObj.Status.GatewayIP = gatewayIP
	_, err = c.egressIPclientset.StaticegressipsV1alpha1().StaticEgressIPs(staticEgressIP.Namespace).Update(copyObj)
	if err != nil {
		glog.Infof("Failed to update GatewayIP to %s for static egress ip %s due to %s\n", gatewayIP, staticEgressIP.Name, err.Error())
	}

	for i, rule := range staticEgressIP.Spec.Rules {
		endpoint, err := c.endpointsLister.Endpoints(staticEgressIP.Namespace).Get(rule.ServiceName)
		if err != nil {
			glog.Errorf("Failed to get endpoints object for service %s due to %s", rule.ServiceName, err.Error())
		}
		ips := make([]string, 0)
		for _, epSubset := range endpoint.Subsets {
			for _, _ = range epSubset.Ports {
				for _, addr := range epSubset.Addresses {
					ips = append(ips, addr.IP)
				}
			}
		}
		err = c.trafficGateway.AddStaticIptablesRule(generateRuleId(staticEgressIP.Namespace, staticEgressIP.Name, i), ips, rule.Cidr, rule.EgressIP)
		if err != nil {
			glog.Errorf("Failed to setup rules to send egress traffic on gateway", err.Error())
		}
		err = gateway.ConfigureStaticIP(rule.EgressIP + "/32")
		if err != nil {
			glog.Errorf("Failed to add egress IP '%s' for the staticegressip '%s' on the gateway due to: '%s'", rule.EgressIP, staticEgressIP.Namespace+"/"+staticEgressIP.Name, err.Error())
		}
	}

	return nil
}

func generateRuleId(namespace, staticEgressIpResourceName string, ruleNo int) string {
	hash := sha256.Sum256([]byte(namespace + staticEgressIpResourceName + strconv.Itoa(ruleNo)))
	encoded := base32.StdEncoding.EncodeToString(hash[:])
	return "EGRESS-IP-" + encoded[:16]
}

func (c *Controller) addStaticEgressIP(obj interface{}) {
	egressIPObj := obj.(*egressipAPI.StaticEgressIP)
	glog.Infof("Adding StaticEgressIP: %s/%s", egressIPObj.Namespace, egressIPObj.Name)
	c.enqueueStaticEgressIP(egressIPObj)

}

func (c *Controller) updateStaticEgressIP(old, current interface{}) {
	oldStaticEgressIPObj := old.(*egressipAPI.StaticEgressIP)
	newStaticEgressIPObj := current.(*egressipAPI.StaticEgressIP)
	glog.Infof("Updating StaticEgressIP: %s/%s", oldStaticEgressIPObj.Namespace, oldStaticEgressIPObj.Name)

	if oldStaticEgressIPObj.Status.GatewayNode != "" && newStaticEgressIPObj.Status.GatewayNode != "" {
		if oldStaticEgressIPObj.Status.GatewayNode != newStaticEgressIPObj.Status.GatewayNode {
			wasGateway, err := c.isEgressGateway(c.kubeclientset, oldStaticEgressIPObj)
			if err != nil {
				glog.Errorf("Failed to identify the gateway for static egress IP %s/%s due to %s", oldStaticEgressIPObj.Namespace, oldStaticEgressIPObj.Name, err.Error())

			}
			isGateway, err := c.isEgressGateway(c.kubeclientset, newStaticEgressIPObj)
			if err != nil {
				glog.Errorf("Failed to identify the gateway for static egress IP %s/%s due to %s", newStaticEgressIPObj.Namespace, newStaticEgressIPObj.Name, err.Error())

			}
			if wasGateway {
				glog.Infof("Transistioning node from gateway to director for the staticegressip: %s/%s\n", newStaticEgressIPObj.Namespace, newStaticEgressIPObj.Name)
			}
			if isGateway {
				glog.Infof("Transistioning node from director to gateway for the staticegressip: %s/%s\n", newStaticEgressIPObj.Namespace, newStaticEgressIPObj.Name)
			}
			for i, rule := range oldStaticEgressIPObj.Spec.Rules {
				endpoint, err := c.endpointsLister.Endpoints(oldStaticEgressIPObj.Namespace).Get(rule.ServiceName)
				if err != nil {
					glog.Errorf("Failed to get endpoints object for service %s due to %s", rule.ServiceName, err.Error())
				}
				ips := make([]string, 0)
				for _, epSubset := range endpoint.Subsets {
					for _, _ = range epSubset.Ports {
						for _, addr := range epSubset.Addresses {
							ips = append(ips, addr.IP)
						}
					}
				}
				if wasGateway {
					err = c.trafficGateway.ClearStaticIptablesRule(generateRuleId(oldStaticEgressIPObj.Namespace, oldStaticEgressIPObj.Name, i), rule.Cidr, rule.EgressIP)
					if err != nil {
						glog.Errorf("Failed to cleanup old rules configured for gateway", err.Error())
					}
					err = gateway.RemoveStaticIP(rule.EgressIP + "/32")
					if err != nil {
						glog.Errorf("Failed to remove egress IP %s for the staticegressip %s on the gateway due to %s", rule.EgressIP, oldStaticEgressIPObj.Namespace+"/"+oldStaticEgressIPObj.Name, err.Error())
					}
				}
				if isGateway {
					err = c.trafficDirector.ClaerStaleRouteToGateway(generateRuleId(oldStaticEgressIPObj.Namespace, oldStaticEgressIPObj.Name, i), rule.Cidr, rule.EgressIP)
					if err != nil {
						glog.Errorf("Failed to cleanup old rules configured for gateway", err.Error())
					}
					err = gateway.RemoveStaticIP(rule.EgressIP + "/32")
					if err != nil {
						glog.Errorf("Failed to add egress IP %s for the staticegressip %s on the gateway due to %s", rule.EgressIP, oldStaticEgressIPObj.Namespace+"/"+oldStaticEgressIPObj.Name, err.Error())
					}
				}

			}
		}
	}
	c.enqueueStaticEgressIP(newStaticEgressIPObj)
}

func (c *Controller) deleteStaticEgressIP(obj interface{}) {
	staticEgressIP, ok := obj.(*egressipAPI.StaticEgressIP)
	if !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("Couldn't get object from tombstone %#v", obj))
			return
		}
		staticEgressIP, ok = tombstone.Obj.(*egressipAPI.StaticEgressIP)
		if !ok {
			runtime.HandleError(fmt.Errorf("Tombstone contained object that is not a Deployment %#v", obj))
			return
		}
	}
	glog.Infof("Deleting StaticEgressIP %s", staticEgressIP.Name)

	for i, rule := range staticEgressIP.Spec.Rules {
		err := c.trafficDirector.DeleteRouteToGateway(generateRuleId(staticEgressIP.Namespace, staticEgressIP.Name, i), rule.Cidr, staticEgressIP.Status.GatewayIP)
		if err != nil {
			glog.Errorf("Failed to delete routes to send the egress traffic to gateway", err.Error())
		}
	}
	c.enqueueStaticEgressIP(staticEgressIP)
}

// enqueueStaticEgressIP takes a StaticEgressIP resource and converts it into a namespace/name
// string which is then put onto the work queue. This method should *not* be
// passed resources of any type other than StaticEgressIP.
func (c *Controller) enqueueStaticEgressIP(egressIP *egressipAPI.StaticEgressIP) {
	key, err := cache.MetaNamespaceKeyFunc(egressIP)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.AddRateLimited(key)
}

// handleObject will take any resource implementing metav1.Object and attempt
// to find the StaticEgressIP resource that 'owns' it. It does this by looking at the
// objects metadata.ownerReferences field for an appropriate OwnerReference.
// It then enqueues that StaticEgressIP resource to be processed. If the object does not
// have an appropriate OwnerReference, it will simply be skipped.
func (c *Controller) handleObject(obj interface{}) {
	var object metav1.Object
	var ok bool
	if object, ok = obj.(metav1.Object); !ok {
		tombstone, ok := obj.(cache.DeletedFinalStateUnknown)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object, invalid type"))
			return
		}
		object, ok = tombstone.Obj.(metav1.Object)
		if !ok {
			runtime.HandleError(fmt.Errorf("error decoding object tombstone, invalid type"))
			return
		}
		glog.V(4).Infof("Recovered deleted object '%s' from tombstone", object.GetName())
	}
	glog.V(4).Infof("Processing object: %s", object.GetName())
}
