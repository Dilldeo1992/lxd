package network

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/big"
	"math/rand"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db"
	deviceConfig "github.com/lxc/lxd/lxd/device/config"
	"github.com/lxc/lxd/lxd/device/nictype"
	"github.com/lxc/lxd/lxd/dnsmasq"
	"github.com/lxc/lxd/lxd/dnsmasq/dhcpalloc"
	"github.com/lxc/lxd/lxd/instance"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/lxd/ip"
	"github.com/lxc/lxd/lxd/project"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/util"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/version"
)

func networkValidPort(value string) error {
	if value == "" {
		return nil
	}

	valueInt, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("Invalid value for an integer: %s", value)
	}

	if valueInt < 1 || valueInt > 65536 {
		return fmt.Errorf("Invalid port number: %s", value)
	}

	return nil
}

// RandomDevName returns a random device name with prefix.
// If the random string combined with the prefix exceeds 13 characters then empty string is returned.
// This is to ensure we support buggy dhclient applications: https://bugs.debian.org/cgi-bin/bugreport.cgi?bug=858580
func RandomDevName(prefix string) string {
	// Return a new random veth device name.
	randBytes := make([]byte, 4)
	rand.Read(randBytes)
	iface := prefix + hex.EncodeToString(randBytes)
	if len(iface) > 13 {
		return ""
	}

	return iface
}

// usedByInstanceDevices looks for instance NIC devices using the network and runs the supplied usageFunc for each.
func usedByInstanceDevices(s *state.State, networkName string, usageFunc func(inst db.Instance, nicName string, nicConfig map[string]string) error) error {
	return s.Cluster.InstanceList(nil, func(inst db.Instance, p db.Project, profiles []api.Profile) error {
		// Look for NIC devices using this network.
		devices := db.ExpandInstanceDevices(deviceConfig.NewDevices(inst.Devices), profiles)
		for devName, devConfig := range devices {
			if isInUseByDevice(networkName, devConfig) {
				err := usageFunc(inst, devName, devConfig)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
}

// UsedBy returns list of API resources using network. Accepts firstOnly argument to indicate that only the first
// resource using network should be returned. This can help to quickly check if the network is in use.
func UsedBy(s *state.State, networkName string, firstOnly bool) ([]string, error) {
	var err error
	var usedBy []string

	// Look for profiles. Cheapest to do.
	var profiles []db.Profile
	err = s.Cluster.Transaction(func(tx *db.ClusterTx) error {
		profiles, err = tx.GetProfiles(db.ProfileFilter{})
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	for _, profile := range profiles {
		inUse, err := usedByProfileDevices(s, profile, networkName)
		if err != nil {
			return nil, err
		}

		if inUse {
			usedBy = append(usedBy, api.NewURL().Path(version.APIVersion, "profiles", profile.Name).Project(profile.Project).String())

			if firstOnly {
				return usedBy, nil
			}
		}
	}

	// Check if any instance devices use this network.
	err = usedByInstanceDevices(s, networkName, func(inst db.Instance, nicName string, nicConfig map[string]string) error {
		usedBy = append(usedBy, api.NewURL().Path(version.APIVersion, "instances", inst.Name).Project(inst.Project).String())

		if firstOnly {
			// No need to consider other devices.
			return db.ErrInstanceListStop
		}

		return nil
	})
	if err != nil {
		if err == db.ErrInstanceListStop {
			return usedBy, nil
		}

		return nil, err
	}

	return usedBy, nil
}

// usedByProfileDevices indicates if network is referenced by a profile's NIC devices.
// Checks if the device's parent or network properties match the network name.
func usedByProfileDevices(s *state.State, profile db.Profile, networkName string) (bool, error) {
	for _, d := range deviceConfig.NewDevices(profile.Devices) {
		if isInUseByDevice(networkName, d) {
			return true, nil
		}
	}

	return false, nil
}

// isInUseByDevices inspects a device's config to find references for a network being used.
func isInUseByDevice(networkName string, d deviceConfig.Device) bool {
	if d["type"] != "nic" {
		return false
	}

	if d["network"] != "" && d["network"] == networkName {
		return true
	}

	if d["parent"] != "" && GetHostDevice(d["parent"], d["vlan"]) == networkName {
		return true
	}

	return false
}

// GetDevMTU retrieves the current MTU setting for a named network device.
func GetDevMTU(devName string) (uint32, error) {
	content, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/mtu", devName))
	if err != nil {
		return 0, err
	}

	// Parse value
	mtu, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 32)
	if err != nil {
		return 0, err
	}

	return uint32(mtu), nil
}

// DefaultGatewaySubnetV4 returns subnet of default gateway interface.
func DefaultGatewaySubnetV4() (*net.IPNet, string, error) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, "", err
	}
	defer file.Close()

	ifaceName := ""

	scanner := bufio.NewReader(file)
	for {
		line, _, err := scanner.ReadLine()
		if err != nil {
			break
		}

		fields := strings.Fields(string(line))

		if fields[1] == "00000000" && fields[7] == "00000000" {
			ifaceName = fields[0]
			break
		}
	}

	if ifaceName == "" {
		return nil, "", fmt.Errorf("No default gateway for IPv4")
	}

	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, "", err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, "", err
	}

	var subnet *net.IPNet

	for _, addr := range addrs {
		addrIP, addrNet, err := net.ParseCIDR(addr.String())
		if err != nil {
			return nil, "", err
		}

		if addrIP.To4() == nil {
			continue
		}

		if subnet != nil {
			return nil, "", fmt.Errorf("More than one IPv4 subnet on default interface")
		}

		subnet = addrNet
	}

	if subnet == nil {
		return nil, "", fmt.Errorf("No IPv4 subnet on default interface")
	}

	return subnet, ifaceName, nil
}

// UpdateDNSMasqStatic rebuilds the DNSMasq static allocations.
func UpdateDNSMasqStatic(s *state.State, networkName string) error {
	// We don't want to race with ourselves here.
	dnsmasq.ConfigMutex.Lock()
	defer dnsmasq.ConfigMutex.Unlock()

	// Get all the networks.
	var networks []string
	if networkName == "" {
		var err error
		networks, err = s.Cluster.GetNetworks()
		if err != nil {
			return err
		}
	} else {
		networks = []string{networkName}
	}

	// Get all the instances.
	insts, err := instance.LoadNodeAll(s, instancetype.Any)
	if err != nil {
		return err
	}

	// Build a list of dhcp host entries.
	entries := map[string][][]string{}
	for _, inst := range insts {
		// Go through all its devices (including profiles).
		for k, d := range inst.ExpandedDevices() {
			// Skip uninteresting entries.
			if d["type"] != "nic" {
				continue
			}

			nicType, err := nictype.NICType(s, d)
			if err != nil || nicType != "bridged" {
				continue
			}

			// Temporarily populate parent from network setting if used.
			if d["network"] != "" {
				d["parent"] = d["network"]
			}

			// Skip devices not connected to managed networks.
			if !shared.StringInSlice(d["parent"], networks) {
				continue
			}

			// Fill in the hwaddr from volatile.
			d, err = inst.FillNetworkDevice(k, d)
			if err != nil {
				continue
			}

			// Add the new host entries.
			_, ok := entries[d["parent"]]
			if !ok {
				entries[d["parent"]] = [][]string{}
			}

			if (shared.IsTrue(d["security.ipv4_filtering"]) && d["ipv4.address"] == "") || (shared.IsTrue(d["security.ipv6_filtering"]) && d["ipv6.address"] == "") {
				_, curIPv4, curIPv6, err := dnsmasq.DHCPStaticAllocation(d["parent"], inst.Project(), inst.Name())
				if err != nil && !os.IsNotExist(err) {
					return err
				}

				if d["ipv4.address"] == "" && curIPv4.IP != nil {
					d["ipv4.address"] = curIPv4.IP.String()
				}

				if d["ipv6.address"] == "" && curIPv6.IP != nil {
					d["ipv6.address"] = curIPv6.IP.String()
				}
			}

			entries[d["parent"]] = append(entries[d["parent"]], []string{d["hwaddr"], inst.Project(), inst.Name(), d["ipv4.address"], d["ipv6.address"]})
		}
	}

	// Update the host files.
	for _, network := range networks {
		entries, _ := entries[network]

		// Skip networks we don't manage (or don't have DHCP enabled).
		if !shared.PathExists(shared.VarPath("networks", network, "dnsmasq.pid")) {
			continue
		}

		n, err := LoadByName(s, network)
		if err != nil {
			return errors.Wrapf(err, "Failed to load network %q in project %q for dnsmasq update", project.Default, network)
		}

		config := n.Config()

		// Wipe everything clean.
		files, err := ioutil.ReadDir(shared.VarPath("networks", network, "dnsmasq.hosts"))
		if err != nil {
			return err
		}

		for _, entry := range files {
			err = os.Remove(shared.VarPath("networks", network, "dnsmasq.hosts", entry.Name()))
			if err != nil {
				return err
			}
		}

		// Apply the changes.
		for entryIdx, entry := range entries {
			hwaddr := entry[0]
			projectName := entry[1]
			cName := entry[2]
			ipv4Address := entry[3]
			ipv6Address := entry[4]
			line := hwaddr

			// Look for duplicates.
			duplicate := false
			for iIdx, i := range entries {
				if project.Instance(entry[1], entry[2]) == project.Instance(i[1], i[2]) {
					// Skip ourselves.
					continue
				}

				if entry[0] == i[0] {
					// Find broken configurations
					logger.Errorf("Duplicate MAC detected: %s and %s", project.Instance(entry[1], entry[2]), project.Instance(i[1], i[2]))
				}

				if i[3] == "" && i[4] == "" {
					// Skip unconfigured.
					continue
				}

				if entry[3] == i[3] && entry[4] == i[4] {
					// Find identical containers (copies with static configuration).
					if entryIdx > iIdx {
						duplicate = true
					} else {
						line = fmt.Sprintf("%s,%s", line, i[0])
						logger.Debugf("Found containers with duplicate IPv4/IPv6: %s and %s", project.Instance(entry[1], entry[2]), project.Instance(i[1], i[2]))
					}
				}
			}

			if duplicate {
				continue
			}

			// Generate the dhcp-host line.
			err := dnsmasq.UpdateStaticEntry(network, projectName, cName, config, hwaddr, ipv4Address, ipv6Address)
			if err != nil {
				return err
			}
		}

		// Signal dnsmasq.
		err = dnsmasq.Kill(network, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// ForkdnsServersList reads the server list file and returns the list as a slice.
func ForkdnsServersList(networkName string) ([]string, error) {
	servers := []string{}
	file, err := os.Open(shared.VarPath("networks", networkName, ForkdnsServersListPath, "/", ForkdnsServersListFile))
	if err != nil {
		return servers, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 {
			servers = append(servers, fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return servers, err
	}

	return servers, nil
}

func randomSubnetV4() (string, error) {
	for i := 0; i < 100; i++ {
		cidr := fmt.Sprintf("10.%d.%d.1/24", rand.Intn(255), rand.Intn(255))
		_, subnet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		if inRoutingTable(subnet) {
			continue
		}

		if pingSubnet(subnet) {
			continue
		}

		return cidr, nil
	}

	return "", fmt.Errorf("Failed to automatically find an unused IPv4 subnet, manual configuration required")
}

func randomSubnetV6() (string, error) {
	for i := 0; i < 100; i++ {
		cidr := fmt.Sprintf("fd42:%x:%x:%x::1/64", rand.Intn(65535), rand.Intn(65535), rand.Intn(65535))
		_, subnet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		if inRoutingTable(subnet) {
			continue
		}

		if pingSubnet(subnet) {
			continue
		}

		return cidr, nil
	}

	return "", fmt.Errorf("Failed to automatically find an unused IPv6 subnet, manual configuration required")
}

func inRoutingTable(subnet *net.IPNet) bool {
	filename := "route"
	if subnet.IP.To4() == nil {
		filename = "ipv6_route"
	}

	file, err := os.Open(fmt.Sprintf("/proc/net/%s", filename))
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewReader(file)
	for {
		line, _, err := scanner.ReadLine()
		if err != nil {
			break
		}

		fields := strings.Fields(string(line))

		// Get the IP
		var ip net.IP
		if filename == "ipv6_route" {
			ip, err = hex.DecodeString(fields[0])
			if err != nil {
				continue
			}
		} else {
			bytes, err := hex.DecodeString(fields[1])
			if err != nil {
				continue
			}

			ip = net.IPv4(bytes[3], bytes[2], bytes[1], bytes[0])
		}

		// Get the mask
		var mask net.IPMask
		if filename == "ipv6_route" {
			size, err := strconv.ParseInt(fmt.Sprintf("0x%s", fields[1]), 0, 64)
			if err != nil {
				continue
			}

			mask = net.CIDRMask(int(size), 128)
		} else {
			bytes, err := hex.DecodeString(fields[7])
			if err != nil {
				continue
			}

			mask = net.IPv4Mask(bytes[3], bytes[2], bytes[1], bytes[0])
		}

		// Generate a new network
		lineNet := net.IPNet{IP: ip, Mask: mask}

		// Ignore default gateway
		if lineNet.IP.Equal(net.ParseIP("::")) {
			continue
		}

		if lineNet.IP.Equal(net.ParseIP("0.0.0.0")) {
			continue
		}

		// Check if we have a route to our new subnet
		if lineNet.Contains(subnet.IP) {
			return true
		}
	}

	return false
}

// pingIP sends a single ping packet to the specified IP, returns true if responds, false if not.
func pingIP(ip net.IP) bool {
	cmd := "ping"
	if ip.To4() == nil {
		cmd = "ping6"
	}

	_, err := shared.RunCommand(cmd, "-n", "-q", ip.String(), "-c", "1", "-W", "1")
	if err != nil {
		// Remote didn't answer.
		return false
	}

	return true
}

func pingSubnet(subnet *net.IPNet) bool {
	var fail bool
	var failLock sync.Mutex
	var wgChecks sync.WaitGroup

	ping := func(ip net.IP) {
		defer wgChecks.Done()

		if !pingIP(ip) {
			return
		}

		// Remote answered
		failLock.Lock()
		fail = true
		failLock.Unlock()
	}

	poke := func(ip net.IP) {
		defer wgChecks.Done()

		addr := fmt.Sprintf("%s:22", ip.String())
		if ip.To4() == nil {
			addr = fmt.Sprintf("[%s]:22", ip.String())
		}

		_, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			// Remote answered
			failLock.Lock()
			fail = true
			failLock.Unlock()
			return
		}
	}

	// Ping first IP
	wgChecks.Add(1)
	go ping(dhcpalloc.GetIP(subnet, 1))

	// Poke port on first IP
	wgChecks.Add(1)
	go poke(dhcpalloc.GetIP(subnet, 1))

	// Ping check
	if subnet.IP.To4() != nil {
		// Ping last IP
		wgChecks.Add(1)
		go ping(dhcpalloc.GetIP(subnet, -2))

		// Poke port on last IP
		wgChecks.Add(1)
		go poke(dhcpalloc.GetIP(subnet, -2))
	}

	wgChecks.Wait()

	return fail
}

// GetHostDevice returns the interface name to use for a combination of parent device name and VLAN ID.
// If no vlan ID supplied, parent name is returned unmodified. If non-empty VLAN ID is supplied then it will look
// for an existing VLAN device and return that, otherwise it will return the default "parent.vlan" format as name.
func GetHostDevice(parent string, vlan string) string {
	// If no VLAN, just use the raw device
	if vlan == "" {
		return parent
	}

	// If no VLANs are configured, use the default pattern
	defaultVlan := fmt.Sprintf("%s.%s", parent, vlan)
	if !shared.PathExists("/proc/net/vlan/config") {
		return defaultVlan
	}

	// Look for an existing VLAN
	f, err := os.Open("/proc/net/vlan/config")
	if err != nil {
		return defaultVlan
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Only grab the lines we're interested in
		s := strings.Split(scanner.Text(), "|")
		if len(s) != 3 {
			continue
		}

		vlanIface := strings.TrimSpace(s[0])
		vlanID := strings.TrimSpace(s[1])
		vlanParent := strings.TrimSpace(s[2])

		if vlanParent == parent && vlanID == vlan {
			return vlanIface
		}
	}

	// Return the default pattern
	return defaultVlan
}

// NeighbourIPState can be { PERMANENT | NOARP | REACHABLE | STALE | NONE | INCOMPLETE | DELAY | PROBE | FAILED }.
type NeighbourIPState string

// NeighbourIPStatePermanent the neighbour entry is valid forever and can be only be removed administratively.
const NeighbourIPStatePermanent = "PERMANENT"

// NeighbourIPStateNoARP the neighbour entry is valid. No attempts to validate this entry will be made but it can
// be removed when its lifetime expires.
const NeighbourIPStateNoARP = "NOARP"

// NeighbourIPStateReachable the neighbour entry is valid until the reachability timeout expires.
const NeighbourIPStateReachable = "REACHABLE"

// NeighbourIPStateStale the neighbour entry is valid but suspicious.
const NeighbourIPStateStale = "STALE"

// NeighbourIPStateNone this is a pseudo state used when initially creating a neighbour entry or after trying to
// remove it before it becomes free to do so.
const NeighbourIPStateNone = "NONE"

// NeighbourIPStateIncomplete the neighbour entry has not (yet) been validated/resolved.
const NeighbourIPStateIncomplete = "INCOMPLETE"

// NeighbourIPStateDelay neighbor entry validation is currently delayed.
const NeighbourIPStateDelay = "DELAY"

// NeighbourIPStateProbe neighbor is being probed.
const NeighbourIPStateProbe = "PROBE"

// NeighbourIPStateFailed max number of probes exceeded without success, neighbor validation has ultimately failed.
const NeighbourIPStateFailed = "FAILED"

// NeighbourIP represents an IP neighbour entry.
type NeighbourIP struct {
	IP    net.IP
	State NeighbourIPState
}

// GetNeighbourIPs returns the IP addresses in the neighbour cache for a particular interface and MAC.
func GetNeighbourIPs(interfaceName string, hwaddr string) ([]NeighbourIP, error) {
	neigh := &ip.Neigh{DevName: interfaceName}
	out, err := neigh.Show()
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to get IP neighbours for interface %q", interfaceName)
	}

	neighbours := []NeighbourIP{}

	for _, line := range strings.Split(out, "\n") {
		// Split fields and early validation.
		fields := strings.Fields(line)
		if len(fields) != 4 {
			continue
		}

		// Check neighbour matches desired MAC address.
		if fields[2] != hwaddr {
			continue
		}

		ip := net.ParseIP(fields[0])
		if ip == nil {
			continue
		}

		neighbours = append(neighbours, NeighbourIP{
			IP:    ip,
			State: NeighbourIPState(fields[3]),
		})
	}

	return neighbours, nil
}

// GetLeaseAddresses returns the lease addresses for a network and hwaddr.
func GetLeaseAddresses(networkName string, hwaddr string) ([]net.IP, error) {
	leaseFile := shared.VarPath("networks", networkName, "dnsmasq.leases")
	if !shared.PathExists(leaseFile) {
		return nil, fmt.Errorf("Leases file not found for network %q", networkName)
	}

	content, err := ioutil.ReadFile(leaseFile)
	if err != nil {
		return nil, err
	}

	addresses := []net.IP{}

	for _, lease := range strings.Split(string(content), "\n") {
		fields := strings.Fields(lease)
		if len(fields) < 5 {
			continue
		}

		// Parse the MAC.
		mac := GetMACSlice(fields[1])
		macStr := strings.Join(mac, ":")

		if len(macStr) < 17 && fields[4] != "" {
			macStr = fields[4][len(fields[4])-17:]
		}

		if macStr != hwaddr {
			continue
		}

		// Parse the IP.
		ip := net.ParseIP(fields[2])
		if ip != nil {
			addresses = append(addresses, ip)
		}
	}

	return addresses, nil
}

// GetMACSlice parses MAC address.
func GetMACSlice(hwaddr string) []string {
	var buf []string

	if !strings.Contains(hwaddr, ":") {
		if s, err := strconv.ParseUint(hwaddr, 10, 64); err == nil {
			hwaddr = fmt.Sprintln(fmt.Sprintf("%x", s))
			var tuple string
			for i, r := range hwaddr {
				tuple = tuple + string(r)
				if i > 0 && (i+1)%2 == 0 {
					buf = append(buf, tuple)
					tuple = ""
				}
			}
		}
	} else {
		buf = strings.Split(strings.ToLower(hwaddr), ":")
	}

	return buf
}

// usesIPv4Firewall returns whether network config will need to use the IPv4 firewall.
func usesIPv4Firewall(netConfig map[string]string) bool {
	if netConfig == nil {
		return false
	}

	if netConfig["ipv4.firewall"] == "" || shared.IsTrue(netConfig["ipv4.firewall"]) {
		return true
	}

	if shared.IsTrue(netConfig["ipv4.nat"]) {
		return true
	}

	return false
}

// usesIPv6Firewall returns whether network config will need to use the IPv6 firewall.
func usesIPv6Firewall(netConfig map[string]string) bool {
	if netConfig == nil {
		return false
	}

	if netConfig["ipv6.firewall"] == "" || shared.IsTrue(netConfig["ipv6.firewall"]) {
		return true
	}

	if shared.IsTrue(netConfig["ipv6.nat"]) {
		return true
	}

	return false
}

// RandomHwaddr generates a random MAC address from the provided random source.
func randomHwaddr(r *rand.Rand) string {
	// Generate a new random MAC address using the usual prefix.
	ret := bytes.Buffer{}
	for _, c := range "00:16:3e:xx:xx:xx" {
		if c == 'x' {
			ret.WriteString(fmt.Sprintf("%x", r.Int31n(16)))
		} else {
			ret.WriteString(string(c))
		}
	}

	return ret.String()
}

// VLANInterfaceCreate creates a VLAN interface on parent interface (if needed).
// Returns boolean indicating if VLAN interface was created.
func VLANInterfaceCreate(parent string, vlanDevice string, vlanID string) (bool, error) {
	if vlanID == "" {
		return false, nil
	}

	if InterfaceExists(vlanDevice) {
		return false, nil
	}

	// Bring the parent interface up so we can add a vlan to it.
	link := &ip.Link{Name: parent}
	err := link.SetUp()
	if err != nil {
		return false, errors.Wrapf(err, "Failed to bring up parent %q", parent)
	}

	// Add VLAN interface on top of parent.
	vlan := &ip.Vlan{
		Link: ip.Link{
			Name:   vlanDevice,
			Parent: parent,
		},
		VlanID: vlanID,
	}

	err = vlan.Add()
	if err != nil {
		return false, errors.Wrapf(err, "Failed to create VLAN interface %q on %q", vlanDevice, parent)
	}

	err = vlan.SetUp()
	if err != nil {
		return false, errors.Wrapf(err, "Failed to bring up interface %q", vlanDevice)
	}

	// Attempt to disable IPv6 router advertisement acceptance.
	util.SysctlSet(fmt.Sprintf("net/ipv6/conf/%s/accept_ra", vlanDevice), "0")

	// We created a new vlan interface, return true.
	return true, nil
}

// InterfaceRemove removes a network interface by name.
func InterfaceRemove(nic string) error {
	link := &ip.Link{Name: nic}
	err := link.Delete()
	return err
}

// InterfaceExists returns true if network interface exists.
func InterfaceExists(nic string) bool {
	if nic != "" && shared.PathExists(fmt.Sprintf("/sys/class/net/%s", nic)) {
		return true
	}

	return false
}

// SubnetContains returns true if outerSubnet contains innerSubnet.
func SubnetContains(outerSubnet *net.IPNet, innerSubnet *net.IPNet) bool {
	if outerSubnet == nil || innerSubnet == nil {
		return false
	}

	if !outerSubnet.Contains(innerSubnet.IP) {
		return false
	}

	outerOnes, outerBits := outerSubnet.Mask.Size()
	innerOnes, innerBits := innerSubnet.Mask.Size()

	// Check number of bits in mask match.
	if innerBits != outerBits {
		return false
	}

	// Check that the inner subnet isn't outside of the outer subnet.
	if innerOnes < outerOnes {
		return false
	}

	return true
}

// SubnetContainsIP returns true if outsetSubnet contains IP address.
func SubnetContainsIP(outerSubnet *net.IPNet, ip net.IP) bool {
	// Convert ip to ipNet.
	ipIsIP4 := ip.To4() != nil

	prefix := 32
	if !ipIsIP4 {
		prefix = 128
	}

	_, ipSubnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ip.String(), prefix))
	if err != nil {
		return false
	}

	ipSubnet.IP = ip

	if SubnetContains(outerSubnet, ipSubnet) {
		return true
	}

	return false
}

// SubnetIterate iterates through each IP in a subnet calling a function for each IP.
// If the ipFunc returns a non-nil error then the iteration stops and the error is returned.
func SubnetIterate(subnet *net.IPNet, ipFunc func(ip net.IP) error) error {
	inc := big.NewInt(1)

	// Convert route start IP to native representations to allow incrementing.
	startIP := subnet.IP.To4()
	if startIP == nil {
		startIP = subnet.IP.To16()
	}

	startBig := big.NewInt(0)
	startBig.SetBytes(startIP)

	// Iterate through IPs in subnet, calling ipFunc for each one.
	for {
		ip := net.IP(startBig.Bytes())
		if !subnet.Contains(ip) {
			break
		}

		err := ipFunc(ip)
		if err != nil {
			return err
		}

		startBig.Add(startBig, inc)
	}

	return nil
}

// SubnetParseAppend parses one or more string CIDR subnets. Appends to the supplied slice. Returns subnets slice.
func SubnetParseAppend(subnets []*net.IPNet, parseSubnet ...string) ([]*net.IPNet, error) {
	for _, subnetStr := range parseSubnet {
		_, subnet, err := net.ParseCIDR(subnetStr)
		if err != nil {
			return nil, errors.Wrapf(err, "Invalid subnet %q", subnetStr)
		}

		subnets = append(subnets, subnet)
	}

	return subnets, nil
}

// InterfaceBindWait waits for network interface to appear after being bound to a driver.
func InterfaceBindWait(ifName string) error {
	for i := 0; i < 10; i++ {
		if InterfaceExists(ifName) {
			return nil
		}

		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("Bind of interface %q took too long", ifName)
}

// IPRangesOverlap checks whether two ip ranges have ip addresses in common
func IPRangesOverlap(r1, r2 *shared.IPRange) bool {
	if r1.End == nil {
		return r2.ContainsIP(r1.Start)
	}

	if r2.End == nil {
		return r1.ContainsIP(r2.Start)
	}

	return r1.ContainsIP(r2.Start) || r1.ContainsIP(r2.End)
}

// InterfaceStatus returns the global unicast IP addresses configured on an interface and whether it is up or not.
func InterfaceStatus(nicName string) ([]net.IP, bool, error) {
	iface, err := net.InterfaceByName(nicName)
	if err != nil {
		return nil, false, errors.Wrapf(err, "Failed loading interface %q", nicName)
	}

	isUp := iface.Flags&net.FlagUp != 0

	addresses, err := iface.Addrs()
	if err != nil {
		return nil, isUp, errors.Wrapf(err, "Failed getting interface addresses for %q", nicName)
	}

	var globalUnicastIPs []net.IP
	for _, address := range addresses {
		ip, _, _ := net.ParseCIDR(address.String())
		if ip == nil {
			continue
		}

		if ip.IsGlobalUnicast() {
			globalUnicastIPs = append(globalUnicastIPs, ip)
		}
	}

	return globalUnicastIPs, isUp, nil
}

// ParsePortRange validates a port range in the form start-end.
func ParsePortRange(r string) (int64, int64, error) {
	entries := strings.Split(r, "-")
	if len(entries) > 2 {
		return -1, -1, fmt.Errorf("Invalid port range %q", r)
	}

	base, err := strconv.ParseInt(entries[0], 10, 64)
	if err != nil {
		return -1, -1, err
	}

	size := int64(1)
	if len(entries) > 1 {
		size, err = strconv.ParseInt(entries[1], 10, 64)
		if err != nil {
			return -1, -1, err
		}

		if size <= base {
			return -1, -1, fmt.Errorf("End port should be higher than start port")
		}

		size -= base
		size++
	}

	return base, size, nil
}

// ParseIPToNet parses a standalone IP address into a net.IPNet (with the IP field set to the IP supplied).
// The address family is detected and the subnet size set to /32 for IPv4 or /128 for IPv6.
func ParseIPToNet(ipAddress string) (*net.IPNet, error) {
	subnetSize := 32
	if strings.Contains(ipAddress, ":") {
		subnetSize = 128
	}

	listenAddress, listenAddressNet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ipAddress, subnetSize))
	if err != nil {
		return nil, err
	}

	listenAddressNet.IP = listenAddress // Add IP back into parsed subnet.

	return listenAddressNet, err
}

// ParseIPCIDRToNet parses an IP in CIDR format into a net.IPNet (with the IP field set to the IP supplied).
func ParseIPCIDRToNet(ipAddressCIDR string) (*net.IPNet, error) {
	listenAddress, listenAddressNet, err := net.ParseCIDR(ipAddressCIDR)
	if err != nil {
		return nil, err
	}

	listenAddressNet.IP = listenAddress // Add IP back into parsed subnet.

	return listenAddressNet, err
}

// NICUsesNetwork returns true if the nicDev's "network" or "parent" property matches one of the networks names.
func NICUsesNetwork(nicDev map[string]string, networks ...*api.Network) bool {
	for _, network := range networks {
		if network.Name == nicDev["network"] || network.Name == nicDev["parent"] {
			return true
		}
	}

	return false
}

// BridgeNetfilterEnabled checks whether the bridge netfilter feature is loaded and enabled.
// If it is not an error is returned. This is needed in order for instances connected to a bridge to access DNAT
// listeners on the LXD host, as otherwise the packets from the bridge do have the SNAT netfilter rules applied.
func BridgeNetfilterEnabled(ipVersion uint) error {
	sysctlName := "iptables"
	if ipVersion == 6 {
		sysctlName = "ip6tables"
	}

	sysctlPath := fmt.Sprintf("net/bridge/bridge-nf-call-%s", sysctlName)
	sysctlVal, err := util.SysctlGet(sysctlPath)
	if err != nil {
		return fmt.Errorf("br_netfilter kernel module not loaded")
	}

	sysctlVal = strings.TrimSpace(sysctlVal)
	if sysctlVal != "1" {
		return fmt.Errorf("sysctl net.bridge.bridge-nf-call-%s not enabled", sysctlName)
	}

	return nil
}
