/*
Copyright 2016 The Rook Authors. All rights reserved.

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

package installer

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
	"testing"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/kubernetes/pkg/kubelet/apis"

	"flag"

	"github.com/coreos/pkg/capnslog"
	"github.com/rook/rook/pkg/apis/ceph.rook.io/v1alpha1"
	cephv1alpha1 "github.com/rook/rook/pkg/apis/ceph.rook.io/v1alpha1"
	rookalpha "github.com/rook/rook/pkg/apis/rook.io/v1alpha2"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/operator/ceph/cluster/osd/config"
	"github.com/rook/rook/pkg/util/exec"
	"github.com/rook/rook/pkg/util/sys"
	"github.com/rook/rook/tests/framework/objects"
	"github.com/rook/rook/tests/framework/utils"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	rookOperatorCreatedCrd = "clusters.ceph.rook.io"
	cockroachDBCRD         = "clusters.cockroachdb.rook.io"
	helmChartName          = "local/rook-ceph"
	helmDeployName         = "rook-ceph"
)

var (
	logger              = capnslog.NewPackageLogger("github.com/rook/rook", "installer")
	createArgs          = []string{"create", "-f"}
	createFromStdinArgs = append(createArgs, "-")
	deleteArgs          = []string{"delete", "-f"}
	deleteFromStdinArgs = append(deleteArgs, "-")
)

//InstallHelper wraps installing and uninstalling rook on a platform
type InstallHelper struct {
	k8shelper   *utils.K8sHelper
	installData *InstallData
	dataDir     string
	helmHelper  *utils.HelmHelper
	Env         objects.EnvironmentManifest
	k8sVersion  string
	T           func() *testing.T
}

func (h *InstallHelper) CreateK8sRookResources() (err error) {
	var resources string
	logger.Info("Creating Rook CRD's")

	resources = h.installData.GetRookCRDs()

	_, err = h.k8shelper.KubectlWithStdin(resources, createFromStdinArgs...)

	return
}

//CreateK8sRookOperator creates rook-operator via kubectl
func (h *InstallHelper) CreateK8sRookOperator(namespace string) (err error) {
	logger.Infof("Starting Rook Operator")
	//creating clusterrolebinding for kubeadm env.
	h.k8shelper.CreateAnonSystemClusterBinding()

	//creating rook resources
	if err = h.CreateK8sRookResources(); err != nil {
		return err
	}

	rookOperator := h.installData.GetRookOperator(namespace)

	_, err = h.k8shelper.KubectlWithStdin(rookOperator, createFromStdinArgs...)
	if err != nil {
		return fmt.Errorf("Failed to create rook-operator pod : %v ", err)
	}

	if !h.k8shelper.IsCRDPresent(rookOperatorCreatedCrd) {
		return fmt.Errorf("Failed to start Rook Operator; k8s CustomResourceDefinition did not appear")
	}

	logger.Infof("Rook Operator started")

	return nil
}

//CreateK8sRookOperatorViaHelm creates rook operator via Helm chart named local/rook present in local repo
func (h *InstallHelper) CreateK8sRookOperatorViaHelm(namespace string) error {
	//creating clusterrolebinding for kubeadm env.
	h.k8shelper.CreateAnonSystemClusterBinding()

	helmTag, err := h.helmHelper.GetLocalRookHelmChartVersion(helmChartName)

	if err != nil {
		return fmt.Errorf("Failed to get Version of helm chart %v, err : %v", helmChartName, err)
	}

	err = h.helmHelper.InstallLocalRookHelmChart(helmChartName, helmDeployName, helmTag, namespace)
	if err != nil {
		return fmt.Errorf("failed toinstall rook operator via helm, err : %v", err)

	}

	if !h.k8shelper.IsCRDPresent(rookOperatorCreatedCrd) {
		return fmt.Errorf("Failed to start Rook Operator; k8s CustomResourceDefinition did not appear")
	}

	return nil
}

//CreateK8sRookToolbox creates rook-ceph-tools via kubectl
func (h *InstallHelper) CreateK8sRookToolbox(namespace string) (err error) {
	logger.Infof("Starting Rook toolbox")

	rookToolbox := h.installData.GetRookToolBox(namespace)

	_, err = h.k8shelper.KubectlWithStdin(rookToolbox, createFromStdinArgs...)

	if err != nil {
		return fmt.Errorf("Failed to create rook-toolbox pod : %v ", err)
	}

	if !h.k8shelper.IsPodRunning("rook-ceph-tools", namespace) {
		return fmt.Errorf("Rook Toolbox couldn't start")
	}
	logger.Infof("Rook Toolbox started")

	return nil
}

func (h *InstallHelper) CreateK8sRookCluster(namespace, systemNamespace string, storeType string) (err error) {
	return h.CreateK8sRookClusterWithHostPathAndDevices(namespace, systemNamespace, storeType, "", false,
		cephv1alpha1.MonSpec{Count: 3, AllowMultiplePerNode: true}, true /* startWithAllNodes */)
}

//CreateK8sRookCluster creates rook cluster via kubectl
func (h *InstallHelper) CreateK8sRookClusterWithHostPathAndDevices(namespace, systemNamespace, storeType, dataDirHostPath string,
	useAllDevices bool, mon cephv1alpha1.MonSpec, startWithAllNodes bool) error {
	if err := os.MkdirAll(DefaultDataDirHostPath(namespace), 0777); err != nil {
		return err
	}

	dataDir, err := ioutil.TempDir(DefaultDataDirHostPath(namespace), "test-")
	if err != nil {
		return err
	}
	dataDirHostPath = dataDir
	h.dataDir = dataDir

	logger.Infof("Creating namespace %s", namespace)
	ns := &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	_, err = h.k8shelper.Clientset.CoreV1().Namespaces().Create(ns)
	if err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create namespace %s. %+v", namespace, err)
	}

	logger.Infof("Creating cluster roles")
	roles := h.installData.GetClusterRoles(namespace, systemNamespace)
	if _, err := h.k8shelper.KubectlWithStdin(roles, createFromStdinArgs...); err != nil {
		return fmt.Errorf("Failed to create cluster roles. %+v", err)
	}

	if h.k8shelper.IsRookClientsetAvailable() {
		logger.Infof("Starting Rook cluster with strongly typed clientset")

		clust := &v1alpha1.Cluster{
			ObjectMeta: metav1.ObjectMeta{
				Name:      namespace,
				Namespace: namespace,
			},
			Spec: v1alpha1.ClusterSpec{
				ServiceAccount:  "rook-ceph-cluster",
				DataDirHostPath: dataDirHostPath,
				Mon: v1alpha1.MonSpec{
					Count:                mon.Count,
					AllowMultiplePerNode: mon.AllowMultiplePerNode,
				},
				Storage: rookalpha.StorageScopeSpec{
					UseAllNodes: startWithAllNodes,
					Selection: rookalpha.Selection{
						UseAllDevices: &useAllDevices,
					},
					Config: map[string]string{
						config.StoreTypeKey:      storeType,
						config.DatabaseSizeMBKey: "1024",
						config.JournalSizeMBKey:  "1024",
					},
				},
			},
		}
		_, err := h.k8shelper.RookClientset.CephV1alpha1().Clusters(namespace).Create(clust)
		if err != nil {
			return fmt.Errorf("failed to create cluster %s. %+v", clust.Name, err)
		}

		if !startWithAllNodes {
			// now that the cluster is created, let's get all the k8s nodes so we can update the cluster CRD with them
			logger.Info("cluster was started without all nodes, will update cluster to add nodes now.")
			k8snodes, err := h.k8shelper.Clientset.CoreV1().Nodes().List(metav1.ListOptions{})
			if err != nil {
				return fmt.Errorf("failed to get k8s nodes to add to cluster CRD: %+v", err)
			}

			// add all discovered k8s nodes to the cluster CRD
			rookNodes := make([]rookalpha.Node, len(k8snodes.Items))
			for i, k8snode := range k8snodes.Items {
				rookNodes[i] = rookalpha.Node{Name: k8snode.Labels[apis.LabelHostname]}
			}
			clust, err = h.k8shelper.RookClientset.CephV1alpha1().Clusters(namespace).Get(namespace, metav1.GetOptions{})
			if err != nil {
				return fmt.Errorf("failed to get rook cluster to add nodes to it: %+v", err)
			}
			clust.Spec.Storage.Nodes = rookNodes

			// update the cluster CRD now
			_, err = h.k8shelper.RookClientset.CephV1alpha1().Clusters(namespace).Update(clust)
			if err != nil {
				return fmt.Errorf("failed to update cluster %s with nodes. %+v", clust.Name, err)
			}
		}
	} else {
		logger.Infof("Starting Rook Cluster with yaml")
		rookCluster := h.installData.GetRookCluster(namespace, storeType, dataDirHostPath, useAllDevices, mon.Count)
		if _, err := h.k8shelper.KubectlWithStdin(rookCluster, createFromStdinArgs...); err != nil {
			return fmt.Errorf("Failed to create rook cluster : %v ", err)
		}
	}

	if err := h.k8shelper.WaitForPodCount("app=rook-ceph-mon", namespace, mon.Count); err != nil {
		return err
	}

	if err := h.k8shelper.WaitForPodCount("app=rook-ceph-osd", namespace, 1); err != nil {
		return err
	}

	logger.Infof("Rook Cluster started")
	err = h.k8shelper.WaitForLabeledPodToRun("app=rook-ceph-osd", namespace)
	return err
}

func SystemNamespace(namespace string) string {
	return fmt.Sprintf("%s-system", namespace)
}

//InstallRookOnK8sWithHostPathAndDevices installs rook on k8s
func (h *InstallHelper) InstallRookOnK8sWithHostPathAndDevices(namespace, storeType, dataDirHostPath string,
	helmInstalled, useDevices bool, mon cephv1alpha1.MonSpec, startWithAllNodes bool) (bool, error) {

	var err error
	//flag used for local debuggin purpose, when rook is pre-installed
	if h.Env.SkipInstallRook {
		return true, nil
	}

	k8sversion := h.k8shelper.GetK8sServerVersion()

	logger.Infof("Installing rook on k8s %s", k8sversion)

	onamespace := namespace
	//Create rook operator
	if helmInstalled {
		err = h.CreateK8sRookOperatorViaHelm(namespace)
		if err != nil {
			logger.Errorf("Rook Operator not installed ,error -> %v", err)
			return false, err

		}
	} else {
		onamespace = SystemNamespace(namespace)

		err := h.CreateK8sRookOperator(onamespace)
		if err != nil {
			logger.Errorf("Rook Operator not installed ,error -> %v", err)
			return false, err
		}
	}
	if !h.k8shelper.IsPodInExpectedState("rook-ceph-operator", onamespace, "Running") {
		logger.Error("rook-ceph-operator is not running")
		h.k8shelper.GetRookLogs("rook-ceph-operator", h.Env.HostType, onamespace, "test-setup")
		logger.Error("rook-ceph-operator is not Running, abort!")
		return false, err
	}

	if useDevices {
		useDevices = IsAdditionalDeviceAvailableOnCluster()
	}

	//Create rook cluster
	err = h.CreateK8sRookClusterWithHostPathAndDevices(namespace, onamespace, storeType, dataDirHostPath,
		useDevices, cephv1alpha1.MonSpec{Count: mon.Count, AllowMultiplePerNode: mon.AllowMultiplePerNode}, startWithAllNodes)
	if err != nil {
		logger.Errorf("Rook cluster %s not installed, error -> %v", namespace, err)
		return false, err
	}

	//Create rook client
	err = h.CreateK8sRookToolbox(namespace)
	if err != nil {
		logger.Errorf("Rook toolbox in cluster %s not installed, error -> %v", namespace, err)
		return false, err
	}
	logger.Infof("installed rook operator and cluster : %s on k8s %s", namespace, h.k8sVersion)
	return true, nil
}

//UninstallRookFromK8s uninstalls rook from k8s
func (h *InstallHelper) UninstallRook(helmInstalled bool, namespace string) {
	h.UninstallRookFromMultipleNS(helmInstalled, SystemNamespace(namespace), namespace)
}

//UninstallRookFromK8s uninstalls rook from multiple namespaces in k8s
func (h *InstallHelper) UninstallRookFromMultipleNS(helmInstalled bool, systemNamespace string, namespaces ...string) {
	//flag used for local debugging purpose, when rook is pre-installed
	if h.Env.SkipInstallRook {
		return
	}

	logger.Infof("Uninstalling Rook")
	var err error
	for _, namespace := range namespaces {

		if !h.k8shelper.VersionAtLeast("v1.8.0") {
			_, err = h.k8shelper.DeleteResource("-n", namespace, "serviceaccount", "rook-ceph-cluster")
			h.checkError(err, "cannot remove serviceaccount rook-ceph-cluster")
			assert.NoError(h.T(), err, "%s  err -> %v", namespace, err)

			err = h.k8shelper.DeleteRoleAndBindings("rook-ceph-cluster", namespace)
			h.checkError(err, "rook-ceph-cluster cluster role and binding cannot be deleted")
			assert.NoError(h.T(), err, "rook-ceph-cluster cluster role and binding cannot be deleted: %+v", err)

			err = h.k8shelper.DeleteRoleBinding("rook-ceph-cluster-mgmt", namespace)
			h.checkError(err, "rook-ceph-cluster-mgmt binding cannot be deleted")
			assert.NoError(h.T(), err, "rook-ceph-cluster-mgmt binding cannot be deleted: %+v", err)
		}

		_, err = h.k8shelper.DeleteResource("-n", namespace, "cluster.ceph.rook.io", namespace)
		h.checkError(err, fmt.Sprintf("cannot remove cluster %s", namespace))

		crdCheckerFunc := func() error {
			_, err := h.k8shelper.RookClientset.RookV1alpha1().Clusters(namespace).Get(namespace, metav1.GetOptions{})
			return err
		}
		err = h.waitForCustomResourceDeletion(namespace, crdCheckerFunc)
		h.checkError(err, fmt.Sprintf("failed to wait for crd %s deletion", namespace))

		_, err = h.k8shelper.DeleteResource("namespace", namespace)
		h.checkError(err, fmt.Sprintf("cannot delete namespace %s", namespace))
	}

	logger.Infof("removing the operator from namespace %s", systemNamespace)
	_, err = h.k8shelper.DeleteResource("crd", "clusters.ceph.rook.io", "pools.ceph.rook.io", "objectstores.ceph.rook.io", "filesystems.ceph.rook.io", "volumes.rook.io")
	h.checkError(err, "cannot delete CRDs")

	if helmInstalled {
		err = h.helmHelper.DeleteLocalRookHelmChart(helmDeployName)
	} else {
		rookOperator := h.installData.GetRookOperator(systemNamespace)
		_, err = h.k8shelper.KubectlWithStdin(rookOperator, deleteFromStdinArgs...)
	}
	h.checkError(err, "cannot uninstall rook-operator")

	h.k8shelper.Clientset.RbacV1beta1().RoleBindings(systemNamespace).Delete("rook-ceph-system", nil)
	h.k8shelper.Clientset.RbacV1beta1().ClusterRoleBindings().Delete("rook-ceph-global", nil)
	h.k8shelper.Clientset.CoreV1().ServiceAccounts(systemNamespace).Delete("rook-ceph-system", nil)
	h.k8shelper.Clientset.RbacV1beta1().ClusterRoles().Delete("rook-ceph-cluster-mgmt", nil)
	h.k8shelper.Clientset.RbacV1beta1().ClusterRoles().Delete("rook-ceph-global", nil)
	h.k8shelper.Clientset.RbacV1beta1().Roles(systemNamespace).Delete("rook-ceph-system", nil)

	logger.Infof("done removing the operator from namespace %s", systemNamespace)
	logger.Infof("removing host data dir %s", h.dataDir)
	// removing data dir if exists
	if len(h.dataDir) > 0 {
		err = h.cleanupDir(h.dataDir)
		logger.Infof("removing %s, err %v", h.dataDir, err)
	}
}

func (h *InstallHelper) cleanupDir(dir string) error {
	resources := h.installData.GetCleanupPod(dir)
	_, err := h.k8shelper.KubectlWithStdin(resources, createArgs...)
	return err
}

func (h *InstallHelper) checkError(err error, message string) {
	// During cleanup the resource might not be found because the test might have failed before the test was done and never created the resource
	if err == nil || errors.IsNotFound(err) {
		return
	}
	assert.NoError(h.T(), err, "%s. %+v", message, err)
}

func (h *InstallHelper) waitForCustomResourceDeletion(namespace string, checkerFunc func() error) error {
	if !h.k8shelper.VersionAtLeast("v1.8.0") {
		// v1.7 has an intermittent issue with long delay to delete resources so we will skip waiting
		return nil
	}

	// wait for the operator to finalize and delete the CRD
	for i := 0; i < 10; i++ {
		err := checkerFunc()
		if err == nil {
			logger.Infof("custom resource %s still exists", namespace)
			time.Sleep(2 * time.Second)
			continue
		}
		if errors.IsNotFound(err) {
			logger.Infof("custom resource %s deleted", namespace)
			return nil
		}
		return err
	}
	logger.Errorf("gave up deleting custom resource %s", namespace)
	return nil
}

//CleanupCluster deletes a rook cluster for a namespace
func (h *InstallHelper) CleanupCluster(clusterName string) {

	logger.Infof("Uninstalling All Rook Clusters - %s", clusterName)
	_, err := h.k8shelper.DeleteResource("-n", clusterName, "cluster", clusterName)
	if err != nil {
		logger.Warningf("Rook Cluster %s cannot be deleted,err -> %v", clusterName, err)
	}

	_, err = h.k8shelper.DeleteResource("-n", clusterName, "serviceaccount", "rook-ceph-osd")
	if err != nil {
		logger.Warningf("rook-ceph-osd service account in namespace %s cannot be deleted,err -> %v", clusterName, err)
	}

	_, err = h.k8shelper.DeleteResource("namespace", clusterName)
	if err != nil {
		logger.Warningf("namespace %s cannot be deleted,err -> %v", clusterName, err)
	}
	if len(h.dataDir) > 0 {
		err = os.RemoveAll(h.dataDir)
		logger.Infof("removing %s, err %v", h.dataDir, err)
	}
}

func (h *InstallHelper) GatherAllRookLogs(nameSpace string, testName string) {
	logger.Infof("Gathering all logs from Rook Cluster %s", nameSpace)
	h.k8shelper.GetRookLogs("rook-ceph-operator", h.Env.HostType, SystemNamespace(nameSpace), testName)
	h.k8shelper.GetRookLogs("rook-ceph-agent", h.Env.HostType, SystemNamespace(nameSpace), testName)
	h.k8shelper.GetRookLogs("rook-ceph-mgr", h.Env.HostType, nameSpace, testName)
	h.k8shelper.GetRookLogs("rook-ceph-mon", h.Env.HostType, nameSpace, testName)
	h.k8shelper.GetRookLogs("rook-ceph-osd", h.Env.HostType, nameSpace, testName)
	h.k8shelper.GetRookLogs("rook-ceph-rgw", h.Env.HostType, nameSpace, testName)
	h.k8shelper.GetRookLogs("rook-ceph-mds", h.Env.HostType, nameSpace, testName)
}

//NewK8sRookhelper creates new instance of InstallHelper
func NewK8sRookhelper(clientset *kubernetes.Clientset, t func() *testing.T) *InstallHelper {

	// All e2e tests should run ceph commands in the toolbox since we are not inside a container
	client.RunAllCephCommandsInToolbox = true

	version, err := clientset.ServerVersion()
	if err != nil {
		logger.Infof("failed to get kubectl server version. %+v", err)
	}

	k8shelp, err := utils.CreateK8sHelper(t)
	if err != nil {
		panic("failed to get kubectl client :" + err.Error())
	}
	ih := &InstallHelper{
		k8shelper:   k8shelp,
		installData: NewK8sInstallData(),
		helmHelper:  utils.NewHelmHelper(),
		Env:         objects.Env,
		k8sVersion:  version.String(),
		T:           t,
	}
	flag.Parse()
	return ih
}

func IsAdditionalDeviceAvailableOnCluster() bool {
	executor := &exec.CommandExecutor{}
	devices, err := sys.ListDevices(executor)
	if err != nil {
		return false
	}
	disks := 0
	logger.Infof("devices : %v", devices)
	for _, device := range devices {
		if strings.Contains(device, "loop") {
			continue
		}
		props, _ := sys.GetDeviceProperties(device, executor)
		if props["TYPE"] == "disk" {
			disks++
		}
	}
	if disks > 1 {
		return true
	}
	logger.Info("No additional disks found on cluster")
	return false
}

func DefaultDataDirHostPath(namespace string) string {
	cwd, _ := os.Getwd()
	testDir := path.Join(cwd, "rook-test", namespace)
	return testDir
}
