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
package ceph

import (
	"fmt"
	"os"
	"strings"

	"github.com/rook/rook/cmd/rook/rook"
	"github.com/rook/rook/pkg/daemon/ceph/client"
	"github.com/rook/rook/pkg/daemon/ceph/mon"
	"github.com/rook/rook/pkg/daemon/ceph/osd"
	"github.com/rook/rook/pkg/operator/ceph/cluster"
	oposd "github.com/rook/rook/pkg/operator/ceph/cluster/osd"
	osdcfg "github.com/rook/rook/pkg/operator/ceph/cluster/osd/config"
	"github.com/rook/rook/pkg/operator/k8sutil"
	"github.com/rook/rook/pkg/util/flags"
	"github.com/spf13/cobra"
)

var osdCmd = &cobra.Command{
	Use:    "osd",
	Short:  "Provisions and runs the osd daemon",
	Hidden: true,
}
var provisionCmd = &cobra.Command{
	Use:    "provision",
	Short:  "Generates osd config and prepares an osd for runtime",
	Hidden: true,
}
var filestoreDeviceCmd = &cobra.Command{
	Use:    "filestore-device",
	Short:  "Runs the ceph daemon for a filestore device",
	Hidden: true,
}
var (
	osdDataDeviceFilter string
	ownerRefID          string
	prepareOnly         bool
	mountSourcePath     string
	mountPath           string
)

func addOSDFlags(command *cobra.Command) {
	provisionCmd.Flags().StringVar(&cfg.devices, "data-devices", "", "comma separated list of devices to use for storage")
	provisionCmd.Flags().StringVar(&ownerRefID, "cluster-id", "", "the UID of the cluster CRD that owns this cluster")
	provisionCmd.Flags().StringVar(&osdDataDeviceFilter, "data-device-filter", "", "a regex filter for the device names to use, or \"all\"")
	provisionCmd.Flags().StringVar(&cfg.directories, "data-directories", "", "comma separated list of directory paths to use for storage")
	provisionCmd.Flags().StringVar(&cfg.metadataDevice, "metadata-device", "", "device to use for metadata (e.g. a high performance SSD/NVMe device)")
	provisionCmd.Flags().StringVar(&cfg.location, "location", "", "location of this node for CRUSH placement")
	provisionCmd.Flags().BoolVar(&cfg.forceFormat, "force-format", false,
		"true to force the format of any specified devices, even if they already have a filesystem.  BE CAREFUL!")
	provisionCmd.Flags().StringVar(&cfg.nodeName, "node-name", os.Getenv("HOSTNAME"), "the host name of the node")

	// OSD store config flags
	provisionCmd.Flags().IntVar(&cfg.storeConfig.WalSizeMB, "osd-wal-size", osdcfg.WalDefaultSizeMB, "default size (MB) for OSD write ahead log (WAL) (bluestore)")
	provisionCmd.Flags().IntVar(&cfg.storeConfig.DatabaseSizeMB, "osd-database-size", osdcfg.DBDefaultSizeMB, "default size (MB) for OSD database (bluestore)")
	provisionCmd.Flags().IntVar(&cfg.storeConfig.JournalSizeMB, "osd-journal-size", osdcfg.JournalDefaultSizeMB, "default size (MB) for OSD journal (filestore)")
	provisionCmd.Flags().StringVar(&cfg.storeConfig.StoreType, "osd-store", "", "type of backing OSD store to use (bluestore or filestore)")

	// only prepare devices but not start ceph-osd daemon
	provisionCmd.Flags().BoolVar(&prepareOnly, "osd-prepare-only", true, "true to only prepare ceph osd directories or devices but not start ceph-osd daemon")

	// flags for running filestore on a device
	filestoreDeviceCmd.Flags().StringVar(&mountSourcePath, "source-path", "", "the source path of the device to mount")
	filestoreDeviceCmd.Flags().StringVar(&mountPath, "mount-path", "", "the path where the device should be mounted")

	// add the subcommands to the parent osd command
	osdCmd.AddCommand(provisionCmd)
	osdCmd.AddCommand(filestoreDeviceCmd)

}

func init() {
	addOSDFlags(osdCmd)
	addCephFlags(osdCmd)
	flags.SetFlagsFromEnv(osdCmd.Flags(), rook.RookEnvVarPrefix)
	flags.SetFlagsFromEnv(provisionCmd.Flags(), rook.RookEnvVarPrefix)
	flags.SetFlagsFromEnv(filestoreDeviceCmd.Flags(), rook.RookEnvVarPrefix)

	provisionCmd.RunE = prepareOSD
	filestoreDeviceCmd.RunE = runFilestoreDeviceOSD
}

// Start the osd daemon for filestore running on a device
func runFilestoreDeviceOSD(cmd *cobra.Command, args []string) error {
	required := []string{"source-path", "mount-path"}
	if err := flags.VerifyRequiredFlags(filestoreDeviceCmd, required); err != nil {
		return err
	}

	args = append(args, []string{
		fmt.Sprintf("--public-addr=%s", cfg.networkInfo.PublicAddrIPv4),
		fmt.Sprintf("--cluster-addr=%s", cfg.networkInfo.ClusterAddrIPv4),
	}...)

	commonOSDInit(filestoreDeviceCmd)

	context := createContext()
	err := osd.RunFilestoreOnDevice(context, mountSourcePath, mountPath, args)
	if err != nil {
		rook.TerminateFatal(err)
	}
	return nil
}

// Provision a device or directory for an OSD
func prepareOSD(cmd *cobra.Command, args []string) error {
	required := []string{"cluster-id", "node-name"}
	if err := flags.VerifyRequiredFlags(provisionCmd, required); err != nil {
		return err
	}
	required = []string{"cluster-name", "mon-endpoints", "mon-secret", "admin-secret"}
	if err := flags.VerifyRequiredFlags(osdCmd, required); err != nil {
		return err
	}

	if err := verifyRenamedFlags(osdCmd); err != nil {
		return err
	}

	var dataDevices string
	var usingDeviceFilter bool
	if osdDataDeviceFilter != "" {
		if cfg.devices != "" {
			return fmt.Errorf("Only one of --data-devices and --data-device-filter can be specified.")
		}

		dataDevices = osdDataDeviceFilter
		usingDeviceFilter = true
	} else {
		dataDevices = cfg.devices
	}

	clientset, _, rookClientset, err := rook.GetClientset()
	if err != nil {
		rook.TerminateFatal(fmt.Errorf("failed to init k8s client. %+v\n", err))
	}

	context := createContext()
	context.Clientset = clientset
	context.RookClientset = rookClientset
	commonOSDInit(provisionCmd)

	locArgs, err := client.FormatLocation(cfg.location, cfg.nodeName)
	if err != nil {
		rook.TerminateFatal(fmt.Errorf("invalid location. %+v\n", err))
	}
	crushLocation := strings.Join(locArgs, " ")

	forceFormat := false
	ownerRef := cluster.ClusterOwnerRef(clusterInfo.Name, ownerRefID)
	kv := k8sutil.NewConfigMapKVStore(clusterInfo.Name, clientset, ownerRef)
	agent := osd.NewAgent(context, dataDevices, usingDeviceFilter, cfg.metadataDevice, cfg.directories, forceFormat,
		crushLocation, cfg.storeConfig, &clusterInfo, cfg.nodeName, kv, prepareOnly)

	err = osd.Provision(context, agent)
	if err != nil {
		// something failed in the OSD orchestration, update the status map with failure details
		status := oposd.OrchestrationStatus{
			Status:  oposd.OrchestrationStatusFailed,
			Message: err.Error(),
		}
		oposd.UpdateOrchestrationStatusMap(clientset, clusterInfo.Name, cfg.nodeName, status)

		rook.TerminateFatal(err)
	}

	return nil
}

func commonOSDInit(cmd *cobra.Command) {
	rook.SetLogLevel()
	rook.LogStartupInfo(cmd.Flags())

	clusterInfo.Monitors = mon.ParseMonEndpoints(cfg.monEndpoints)
}
