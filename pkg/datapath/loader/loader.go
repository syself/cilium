// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package loader

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/byteorder"
	"github.com/cilium/cilium/pkg/datapath/link"
	"github.com/cilium/cilium/pkg/datapath/linux/linux_defaults"
	"github.com/cilium/cilium/pkg/datapath/linux/route"
	"github.com/cilium/cilium/pkg/datapath/linux/sysctl"
	"github.com/cilium/cilium/pkg/datapath/loader/metrics"
	"github.com/cilium/cilium/pkg/datapath/tables"
	datapath "github.com/cilium/cilium/pkg/datapath/types"
	"github.com/cilium/cilium/pkg/defaults"
	"github.com/cilium/cilium/pkg/elf"
	"github.com/cilium/cilium/pkg/hive/cell"
	iputil "github.com/cilium/cilium/pkg/ip"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/maps/callsmap"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/statedb"
	wgTypes "github.com/cilium/cilium/pkg/wireguard/types"
)

const (
	subsystem = "datapath-loader"

	symbolFromEndpoint = "cil_from_container"
	symbolToEndpoint   = "cil_to_container"
	symbolFromNetwork  = "cil_from_network"

	symbolFromHostNetdevEp = "cil_from_netdev"
	symbolToHostNetdevEp   = "cil_to_netdev"
	symbolFromHostEp       = "cil_from_host"
	symbolToHostEp         = "cil_to_host"

	symbolFromHostNetdevXDP = "cil_xdp_entry"

	symbolFromOverlay = "cil_from_overlay"
	symbolToOverlay   = "cil_to_overlay"

	dirIngress = "ingress"
	dirEgress  = "egress"
)

const (
	secctxFromIpcacheDisabled = iota + 1
	secctxFromIpcacheEnabled
)

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, subsystem)

// loader is a wrapper structure around operations related to compiling,
// loading, and reloading datapath programs.
type loader struct {
	cfg Config

	once sync.Once

	// templateCache is the cache of pre-compiled datapaths.
	templateCache *objectCache

	ipsecMu lock.Mutex // guards reinitializeIPSec

	hostDpInitializedOnce sync.Once
	hostDpInitialized     chan struct{}

	sysctl    sysctl.Sysctl
	db        *statedb.DB
	nodeAddrs statedb.Table[tables.NodeAddress]
	devices   statedb.Table[*tables.Device]
}

type Params struct {
	cell.In

	Config    Config
	DB        *statedb.DB
	NodeAddrs statedb.Table[tables.NodeAddress]
	Sysctl    sysctl.Sysctl
	Devices   statedb.Table[*tables.Device]
}

// newLoader returns a new loader.
func newLoader(p Params) *loader {
	return &loader{
		cfg:               p.Config,
		db:                p.DB,
		nodeAddrs:         p.NodeAddrs,
		sysctl:            p.Sysctl,
		devices:           p.Devices,
		hostDpInitialized: make(chan struct{}),
	}
}

func NewLoaderForTest(tb testing.TB) *loader {
	nodeAddrs, err := tables.NewNodeAddressTable()
	require.NoError(tb, err, "NewNodeAddressTable")
	devices, err := tables.NewDeviceTable()
	require.NoError(tb, err, "NewDeviceTable")
	db, err := statedb.NewDB([]statedb.TableMeta{nodeAddrs, devices}, statedb.NewMetrics())
	require.NoError(tb, err, "NewDB")
	return newLoader(Params{
		Config:    DefaultConfig,
		DB:        db,
		NodeAddrs: nodeAddrs,
		Sysctl:    sysctl.NewDirectSysctl(afero.NewOsFs(), "/proc"),
		Devices:   devices,
	})
}

// Init initializes the datapath cache with base program hashes derived from
// the LocalNodeConfiguration.
func (l *loader) init(dp datapath.ConfigWriter, nodeCfg *datapath.LocalNodeConfiguration) {
	l.once.Do(func() {
		l.templateCache = newObjectCache(dp, nodeCfg, option.Config.StateDir)
		ignorePrefixes := ignoredELFPrefixes
		if !option.Config.EnableIPv4 {
			ignorePrefixes = append(ignorePrefixes, "LXC_IPV4")
		}
		elf.IgnoreSymbolPrefixes(ignorePrefixes)
	})
	l.templateCache.Update(nodeCfg)
}

func upsertEndpointRoute(ep datapath.Endpoint, ip net.IPNet) error {
	endpointRoute := route.Route{
		Prefix: ip,
		Device: ep.InterfaceName(),
		Scope:  netlink.SCOPE_LINK,
		Proto:  linux_defaults.RTProto,
	}

	return route.Upsert(endpointRoute)
}

func removeEndpointRoute(ep datapath.Endpoint, ip net.IPNet) error {
	return route.Delete(route.Route{
		Prefix: ip,
		Device: ep.InterfaceName(),
		Scope:  netlink.SCOPE_LINK,
	})
}

// We need this function when patching an object file for which symbols were
// already substituted. During the first symbol substitutions, string symbols
// were replaced such that:
//
//	template_string -> string_for_endpoint
//
// Since we only want to replace one int symbol, we can nullify string
// substitutions with:
//
//	string_for_endpoint -> string_for_endpoint
//
// We cannot simply pass an empty map as the agent would complain that some
// symbol had no corresponding values.
func nullifyStringSubstitutions(strings map[string]string) map[string]string {
	nullStrings := make(map[string]string)
	for _, v := range strings {
		nullStrings[v] = v
	}
	return nullStrings
}

func (l *loader) bpfMasqAddrs(ifName string) (masq4, masq6 netip.Addr) {
	if l.cfg.DeriveMasqIPAddrFromDevice != "" {
		ifName = l.cfg.DeriveMasqIPAddrFromDevice
	}

	find := func(iter statedb.Iterator[tables.NodeAddress]) bool {
		for addr, _, ok := iter.Next(); ok; addr, _, ok = iter.Next() {
			if !addr.Primary {
				continue
			}
			if addr.Addr.Is4() && !masq4.IsValid() {
				masq4 = addr.Addr
			} else if addr.Addr.Is6() && !masq6.IsValid() {
				masq6 = addr.Addr
			}
			done := (!option.Config.EnableIPv4Masquerade || masq4.IsValid()) &&
				(!option.Config.EnableIPv6Masquerade || masq6.IsValid())
			if done {
				return true
			}
		}
		return false
	}

	// Try to find suitable masquerade address first from the given interface.
	txn := l.db.ReadTxn()
	iter, _ := l.nodeAddrs.Get(
		txn,
		tables.NodeAddressDeviceNameIndex.Query(ifName),
	)
	if !find(iter) {
		// No suitable masquerade addresses were found for this device. Try the fallback
		// addresses.
		iter, _ = l.nodeAddrs.Get(
			txn,
			tables.NodeAddressDeviceNameIndex.Query(tables.WildcardDeviceName),
		)
		find(iter)
	}

	return
}

// Since we attach the host endpoint datapath to two different interfaces, we
// need two different NODE_MAC values. patchHostNetdevDatapath creates a new
// object file for the native device, from the object file for the host device
// (cilium_host).
// Since the two object files should only differ by the values of their
// NODE_MAC symbols, we can avoid a full compilation.
func (l *loader) patchHostNetdevDatapath(ep datapath.Endpoint, objPath, dstPath, ifName string) error {
	hostObj, err := elf.Open(objPath)
	if err != nil {
		return err
	}
	defer hostObj.Close()

	opts, strings := l.ELFSubstitutions(ep)

	// The NODE_MAC value is specific to each attachment interface.
	mac, err := link.GetHardwareAddr(ifName)
	if err != nil {
		return err
	}
	if mac == nil {
		// L2-less device
		mac = make([]byte, 6)
		opts["ETH_HLEN"] = uint64(0)
	}
	opts["NODE_MAC_1"] = uint64(sliceToBe32(mac[0:4]))
	opts["NODE_MAC_2"] = uint64(sliceToBe16(mac[4:6]))

	ifIndex, err := link.GetIfIndex(ifName)
	if err != nil {
		return err
	}

	if !option.Config.EnableHostLegacyRouting {
		opts["SECCTX_FROM_IPCACHE"] = uint64(secctxFromIpcacheEnabled)
	} else {
		opts["SECCTX_FROM_IPCACHE"] = uint64(secctxFromIpcacheDisabled)
	}

	if option.Config.EnableNodePort {
		opts["NATIVE_DEV_IFINDEX"] = uint64(ifIndex)
	}
	if option.Config.EnableBPFMasquerade && ifName != defaults.SecondHostDevice {
		ipv4, ipv6 := l.bpfMasqAddrs(ifName)

		if option.Config.EnableIPv4Masquerade && ipv4.IsValid() {
			opts["IPV4_MASQUERADE"] = uint64(byteorder.NetIPv4ToHost32(ipv4.AsSlice()))
		}
		if option.Config.EnableIPv6Masquerade && ipv6.IsValid() {
			ipv6Bytes := ipv6.AsSlice()
			opts["IPV6_MASQUERADE_1"] = sliceToBe64(ipv6Bytes[0:8])
			opts["IPV6_MASQUERADE_2"] = sliceToBe64(ipv6Bytes[8:16])
		}
	}

	// Among string substitutions, only the calls map name is specific to each
	// attachment interface.
	strings = nullifyStringSubstitutions(strings)
	callsMapHostDevice := bpf.LocalMapName(callsmap.HostMapName, uint16(ep.GetID()))
	strings[callsMapHostDevice] = bpf.LocalMapName(callsmap.NetdevMapName, uint16(ifIndex))

	return hostObj.Write(dstPath, opts, strings)
}

func isObsoleteDev(dev string, devices []string) bool {
	// exclude devices we never attach to/from_netdev to.
	for _, prefix := range defaults.ExcludedDevicePrefixes {
		if strings.HasPrefix(dev, prefix) {
			return false
		}
	}

	// exclude devices that will still be managed going forward.
	for _, d := range devices {
		if dev == d {
			return false
		}
	}

	return true
}

// removeObsoleteNetdevPrograms removes cil_to_netdev and cil_from_netdev from devices
// that cilium potentially doesn't manage anymore after a restart, e.g. if the set of
// devices changes between restarts.
//
// This code assumes that the agent was upgraded from a prior version while maintaining
// the same list of managed physical devices. This ensures that all tc bpf filters get
// replaced using the naming convention of the 'current' agent build. For example,
// before 1.13, most filters were named e.g. bpf_host.o:[to-host], to be changed to
// cilium-<device> in 1.13, then to cil_to_host-<device> in 1.14. As a result, this
// function only cleans up filters following the current naming scheme.
func removeObsoleteNetdevPrograms(devices []string) error {
	links, err := netlink.LinkList()
	if err != nil {
		return fmt.Errorf("retrieving all netlink devices: %w", err)
	}

	// collect all devices that have netdev programs attached on either ingress or egress.
	ingressDevs := []netlink.Link{}
	egressDevs := []netlink.Link{}
	for _, l := range links {
		if !isObsoleteDev(l.Attrs().Name, devices) {
			continue
		}

		ingressFilters, err := netlink.FilterList(l, directionToParent(dirIngress))
		if err != nil {
			return fmt.Errorf("listing ingress filters: %w", err)
		}
		for _, filter := range ingressFilters {
			if bpfFilter, ok := filter.(*netlink.BpfFilter); ok {
				if strings.HasPrefix(bpfFilter.Name, symbolFromHostNetdevEp) {
					ingressDevs = append(ingressDevs, l)
				}
			}
		}

		egressFilters, err := netlink.FilterList(l, directionToParent(dirEgress))
		if err != nil {
			return fmt.Errorf("listing egress filters: %w", err)
		}
		for _, filter := range egressFilters {
			if bpfFilter, ok := filter.(*netlink.BpfFilter); ok {
				if strings.HasPrefix(bpfFilter.Name, symbolToHostNetdevEp) {
					egressDevs = append(egressDevs, l)
				}
			}
		}
	}

	for _, dev := range ingressDevs {
		err = removeTCFilters(dev.Attrs().Name, directionToParent(dirIngress))
		if err != nil {
			log.WithError(err).Errorf("couldn't remove ingress tc filters from %s", dev.Attrs().Name)
		}
	}

	for _, dev := range egressDevs {
		err = removeTCFilters(dev.Attrs().Name, directionToParent(dirEgress))
		if err != nil {
			log.WithError(err).Errorf("couldn't remove egress tc filters from %s", dev.Attrs().Name)
		}
	}

	return nil
}

// reloadHostDatapath (re)attaches BPF programs to:
// - cilium_host: ingress and egress
// - cilium_net: ingress
// - native devices: ingress and (optionally) egress if certain features require it
func (l *loader) reloadHostDatapath(ctx context.Context, ep datapath.Endpoint, objPath string, devices []string) error {
	// Warning: here be dragons. There used to be a single loop over
	// interfaces+objs+progs here from the iproute2 days, but this was never
	// correct to begin with. Tail call maps were always reused when possible,
	// causing control flow to transition through invalid states as new tail calls
	// were sequentially upserted into the array.
	//
	// Take care not to call replaceDatapath() twice for a single ELF/interface.
	// Map migration should only be run once per ELF, otherwise cilium_calls_*
	// created by prior loads will be unpinned, causing them to be emptied,
	// missing all tail calls.

	// Replace programs on cilium_host.
	host, err := netlink.LinkByName(ep.InterfaceName())
	if err != nil {
		return fmt.Errorf("retrieving device %s: %w", ep.InterfaceName(), err)
	}

	progs := []progDefinition{
		{progName: symbolToHostEp, direction: dirIngress},
		{progName: symbolFromHostEp, direction: dirEgress},
	}
	finalize, err := replaceDatapath(ctx,
		replaceDatapathOptions{
			device:   ep.InterfaceName(),
			elf:      objPath,
			programs: progs,
			linkDir:  bpffsDeviceLinksDir(bpf.CiliumPath(), host),
		},
	)
	if err != nil {
		scopedLog := ep.Logger(subsystem).WithFields(logrus.Fields{
			logfields.Path: objPath,
			logfields.Veth: ep.InterfaceName(),
		})
		// Don't log an error here if the context was canceled or timed out;
		// this log message should only represent failures with respect to
		// loading the program.
		if ctx.Err() == nil {
			scopedLog.WithError(err).Warningf("JoinEP: Failed to load program for %s", ep.InterfaceName())
		}
		return err
	}
	// Defer map removal until all interfaces' progs have been replaced.
	defer finalize()

	// Replace program on cilium_net.
	net, err := netlink.LinkByName(defaults.SecondHostDevice)
	if err != nil {
		log.WithError(err).WithField("device", defaults.SecondHostDevice).Error("Link does not exist")
		return fmt.Errorf("device '%s' not found: %w", defaults.SecondHostDevice, err)
	}

	secondDevObjPath := path.Join(ep.StateDir(), hostEndpointPrefix+"_"+defaults.SecondHostDevice+".o")
	if err := l.patchHostNetdevDatapath(ep, objPath, secondDevObjPath, defaults.SecondHostDevice); err != nil {
		return err
	}

	progs = []progDefinition{
		{progName: symbolToHostEp, direction: dirIngress},
	}

	finalize, err = replaceDatapath(ctx,
		replaceDatapathOptions{
			device:   defaults.SecondHostDevice,
			elf:      secondDevObjPath,
			programs: progs,
			linkDir:  bpffsDeviceLinksDir(bpf.CiliumPath(), net),
		},
	)
	if err != nil {
		scopedLog := ep.Logger(subsystem).WithFields(logrus.Fields{
			logfields.Path: objPath,
			logfields.Veth: defaults.SecondHostDevice,
		})
		if ctx.Err() == nil {
			scopedLog.WithError(err).Warningf("JoinEP: Failed to load program for %s", defaults.SecondHostDevice)
		}
		return err
	}
	defer finalize()

	// Replace programs on physical devices.
	for _, device := range devices {
		iface, err := netlink.LinkByName(device)
		if err != nil {
			log.WithError(err).WithField("device", device).Warn("Link does not exist")
			continue
		}

		netdevObjPath := path.Join(ep.StateDir(), hostEndpointNetdevPrefix+device+".o")
		if err := l.patchHostNetdevDatapath(ep, objPath, netdevObjPath, device); err != nil {
			return err
		}

		progs := []progDefinition{
			{progName: symbolFromHostNetdevEp, direction: dirIngress},
		}

		if option.Config.AreDevicesRequired() &&
			// Attaching bpf_host to cilium_wg0 is required for encrypting KPR
			// traffic. Only ingress prog (aka "from-netdev") is needed to handle
			// the rev-NAT xlations.
			device != wgTypes.IfaceName {

			progs = append(progs, progDefinition{symbolToHostNetdevEp, dirEgress})
		} else {
			// Remove any previously attached device from egress path if BPF
			// NodePort and host firewall are disabled.
			err := removeTCFilters(device, netlink.HANDLE_MIN_EGRESS)
			if err != nil {
				log.WithField("device", device).Error(err)
			}
		}

		finalize, err := replaceDatapath(ctx,
			replaceDatapathOptions{
				device:   device,
				elf:      netdevObjPath,
				programs: progs,
				linkDir:  bpffsDeviceLinksDir(bpf.CiliumPath(), iface),
			},
		)
		if err != nil {
			scopedLog := ep.Logger(subsystem).WithFields(logrus.Fields{
				logfields.Path: objPath,
				logfields.Veth: device,
			})
			if ctx.Err() == nil {
				scopedLog.WithError(err).Warningf("JoinEP: Failed to load program for physical device %s", device)
			}
			return err
		}
		defer finalize()
	}

	// call at the end of the function so that we can easily detect if this removes necessary
	// programs that have just been attached.
	if err := removeObsoleteNetdevPrograms(devices); err != nil {
		log.WithError(err).Error("Failed to remove obsolete netdev programs")
	}

	l.hostDpInitializedOnce.Do(func() {
		log.Debug("Initialized host datapath")
		close(l.hostDpInitialized)
	})

	return nil
}

func (l *loader) reloadDatapath(ctx context.Context, ep datapath.Endpoint, dirs *directoryInfo) error {
	// Replace the current program
	objPath := path.Join(dirs.Output, endpointObj)

	if ep.IsHost() {
		// TODO: react to changes (using the currently ignored watch channel)
		nativeDevices, _ := tables.SelectedDevices(l.devices, l.db.ReadTxn())
		devices := tables.DeviceNames(nativeDevices)

		if option.Config.NeedBPFHostOnWireGuardDevice() {
			devices = append(devices, wgTypes.IfaceName)
		}

		objPath = path.Join(dirs.Output, hostEndpointObj)
		if err := l.reloadHostDatapath(ctx, ep, objPath, devices); err != nil {
			return err
		}
	} else {
		progs := []progDefinition{{progName: symbolFromEndpoint, direction: dirIngress}}

		if ep.RequireEgressProg() {
			progs = append(progs, progDefinition{progName: symbolToEndpoint, direction: dirEgress})
		} else {
			err := removeTCFilters(ep.InterfaceName(), netlink.HANDLE_MIN_EGRESS)
			if err != nil {
				log.WithField("device", ep.InterfaceName()).Error(err)
			}
		}

		finalize, err := replaceDatapath(ctx,
			replaceDatapathOptions{
				device:   ep.InterfaceName(),
				elf:      objPath,
				programs: progs,
				linkDir:  bpffsEndpointLinksDir(bpf.CiliumPath(), ep),
			},
		)
		if err != nil {
			scopedLog := ep.Logger(subsystem).WithFields(logrus.Fields{
				logfields.Path: objPath,
				logfields.Veth: ep.InterfaceName(),
			})
			// Don't log an error here if the context was canceled or timed out;
			// this log message should only represent failures with respect to
			// loading the program.
			if ctx.Err() == nil {
				scopedLog.WithError(err).Warn("JoinEP: Failed to attach program(s)")
			}
			return err
		}
		defer finalize()
	}

	if ep.RequireEndpointRoute() {
		scopedLog := ep.Logger(subsystem).WithFields(logrus.Fields{
			logfields.Veth: ep.InterfaceName(),
		})
		if ip := ep.IPv4Address(); ip.IsValid() {
			if err := upsertEndpointRoute(ep, *iputil.AddrToIPNet(ip)); err != nil {
				scopedLog.WithError(err).Warn("Failed to upsert route")
			}
		}
		if ip := ep.IPv6Address(); ip.IsValid() {
			if err := upsertEndpointRoute(ep, *iputil.AddrToIPNet(ip)); err != nil {
				scopedLog.WithError(err).Warn("Failed to upsert route")
			}
		}
	}

	return nil
}

func (l *loader) replaceOverlayDatapath(ctx context.Context, cArgs []string, iface string) error {
	if err := compileOverlay(ctx, cArgs); err != nil {
		log.WithError(err).Fatal("failed to compile overlay programs")
	}

	device, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("retrieving device %s: %w", iface, err)
	}

	progs := []progDefinition{
		{progName: symbolFromOverlay, direction: dirIngress},
		{progName: symbolToOverlay, direction: dirEgress},
	}

	finalize, err := replaceDatapath(ctx,
		replaceDatapathOptions{
			device:   iface,
			elf:      overlayObj,
			programs: progs,
			linkDir:  bpffsDeviceLinksDir(bpf.CiliumPath(), device),
		},
	)
	if err != nil {
		log.WithField(logfields.Interface, iface).WithError(err).Fatal("Load overlay network failed")
	}
	finalize()

	return nil
}

// CompileOrLoad loads the BPF datapath programs for the specified endpoint.
//
// It attempts to find a pre-compiled
// template datapath object to use, to avoid a costly compile operation.
// Only if there is no existing template that has the same configuration
// parameters as the specified endpoint, this function will compile a new
// template for this configuration.
//
// This function will block if the cache does not contain an entry for the
// same EndpointConfiguration and multiple goroutines attempt to concurrently
// CompileOrLoad with the same configuration parameters. When the first
// goroutine completes compilation of the template, all other CompileOrLoad
// invocations will be released.
func (l *loader) CompileOrLoad(ctx context.Context, ep datapath.Endpoint, stats *metrics.SpanStat) error {
	dirs := &directoryInfo{
		Library: option.Config.BpfDir,
		Runtime: option.Config.StateDir,
		State:   ep.StateDir(),
		Output:  ep.StateDir(),
	}
	return l.compileOrLoad(ctx, ep, dirs, stats)
}

func (l *loader) compileOrLoad(ctx context.Context, ep datapath.Endpoint, dirs *directoryInfo, stats *metrics.SpanStat) error {
	templateFile, _, err := l.templateCache.fetchOrCompile(ctx, ep, dirs, stats)
	if err != nil {
		return err
	}
	defer templateFile.Close()

	template, err := elf.NewELF(templateFile, ep.Logger(subsystem))
	if err != nil {
		return err
	}
	defer template.Close()

	symPath := path.Join(ep.StateDir(), defaults.TemplatePath)
	if _, err := os.Stat(symPath); err == nil {
		if err = os.RemoveAll(symPath); err != nil {
			return &os.PathError{
				Op:   "Failed to remove old symlink",
				Path: symPath,
				Err:  err,
			}
		}
	} else if !os.IsNotExist(err) {
		return &os.PathError{
			Op:   "Failed to locate symlink",
			Path: symPath,
			Err:  err,
		}
	}
	if err := os.Symlink(templateFile.Name(), symPath); err != nil {
		return &os.PathError{
			Op:   fmt.Sprintf("Failed to create symlink to %s", templateFile.Name()),
			Path: symPath,
			Err:  err,
		}
	}

	stats.BpfWriteELF.Start()
	epObj := endpointObj
	if ep.IsHost() {
		epObj = hostEndpointObj
	}
	dstPath := path.Join(ep.StateDir(), epObj)
	opts, strings := l.ELFSubstitutions(ep)
	if err = template.Write(dstPath, opts, strings); err != nil {
		stats.BpfWriteELF.End(err == nil)
		return err
	}
	stats.BpfWriteELF.End(err == nil)

	return l.ReloadDatapath(ctx, ep, stats)
}

// ReloadDatapath reloads the BPF datapath programs for the specified endpoint.
func (l *loader) ReloadDatapath(ctx context.Context, ep datapath.Endpoint, stats *metrics.SpanStat) (err error) {
	dirs := directoryInfo{
		Library: option.Config.BpfDir,
		Runtime: option.Config.StateDir,
		State:   ep.StateDir(),
		Output:  ep.StateDir(),
	}
	stats.BpfLoadProg.Start()
	err = l.reloadDatapath(ctx, ep, &dirs)
	stats.BpfLoadProg.End(err == nil)
	return err
}

// Unload removes the datapath specific program aspects
func (l *loader) Unload(ep datapath.Endpoint) {
	if ep.RequireEndpointRoute() {
		if ip := ep.IPv4Address(); ip.IsValid() {
			removeEndpointRoute(ep, *iputil.AddrToIPNet(ip))
		}

		if ip := ep.IPv6Address(); ip.IsValid() {
			removeEndpointRoute(ep, *iputil.AddrToIPNet(ip))
		}
	}

	// If Cilium and the kernel support tcx to attach TC programs to the
	// endpoint's veth device, its bpf_link object is pinned to a per-endpoint
	// bpffs directory. When the endpoint gets deleted, removing the whole
	// directory cleans up any pinned maps and links.
	bpffsPath := bpffsEndpointDir(bpf.CiliumPath(), ep)
	if err := bpf.Remove(bpffsPath); err != nil {
		log.WithError(err).WithField(logfields.EndpointID, ep.StringID())
	}
}

// EndpointHash hashes the specified endpoint configuration with the current
// datapath hash cache and returns the hash as string.
func (l *loader) EndpointHash(cfg datapath.EndpointConfiguration) (string, error) {
	return l.templateCache.baseHash.sumEndpoint(l.templateCache, cfg, true)
}

// CallsMapPath gets the BPF Calls Map for the endpoint with the specified ID.
func (l *loader) CallsMapPath(id uint16) string {
	return bpf.LocalMapPath(callsmap.MapName, id)
}

// CustomCallsMapPath gets the BPF Custom Calls Map for the endpoint with the
// specified ID.
func (l *loader) CustomCallsMapPath(id uint16) string {
	return bpf.LocalMapPath(callsmap.CustomCallsMapName, id)
}

// HostDatapathInitialized returns a channel which is closed when the
// host datapath has been loaded for the first time.
func (l *loader) HostDatapathInitialized() <-chan struct{} {
	return l.hostDpInitialized
}
