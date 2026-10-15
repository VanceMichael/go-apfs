package apfs

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/blacktop/go-apfs/pkg/disk"
	"github.com/blacktop/go-apfs/types"
)

// fakeDevice is an immutable block device backed by a map. Missing blocks
// read back as zero bytes.
type fakeDevice struct {
	blocks map[uint64][]byte
}

func (d *fakeDevice) ReadAt(p []byte, off int64) (int, error) {
	addr := uint64(off) / types.BLOCK_SIZE
	start := uint64(off) % types.BLOCK_SIZE
	blk, ok := d.blocks[addr]
	if !ok {
		// sparse hole: zero-filled block, like a real device past EOF
		for i := range p {
			p[i] = 0
		}
		return len(p), nil
	}
	n := copy(p, blk[start:])
	if n < len(p) {
		for i := n; i < len(p); i++ {
			p[i] = 0
		}
	}
	return len(p), nil
}

func sealBlock(buf []byte) []byte {
	binary.LittleEndian.PutUint64(buf[0:8], 0)
	csum := types.CreateChecksum(buf)
	binary.LittleEndian.PutUint64(buf[0:8], csum)
	return buf
}

func writeObjHeader(buf []byte, hdr types.ObjPhysT) {
	binary.LittleEndian.PutUint64(buf[8:16], uint64(hdr.Oid))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(hdr.Xid))
	binary.LittleEndian.PutUint32(buf[24:28], uint32(hdr.Type))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(hdr.Subtype))
}

// putFSRoot emits an empty root leaf B-tree node.
func putFSRoot(blocks map[uint64][]byte, addr, oid, xid uint64) {
	buf := make([]byte, types.BLOCK_SIZE)
	writeObjHeader(buf, types.ObjPhysT{Oid: types.OidT(oid), Xid: types.XidT(xid), Type: types.OBJECT_TYPE_BTREE, Subtype: types.OBJECT_TYPE_FSTREE})
	binary.LittleEndian.PutUint16(buf[32:34], uint16(types.BTNODE_ROOT|types.BTNODE_LEAF))
	blocks[addr] = sealBlock(buf)
}

type omapMapping struct {
	oid, xid, paddr uint64
}

// putOMapLeaf emits a fixed-KV leaf node of the object map B-tree. The block
// layout is exactly what BTreeNodePhys.ReadOMapNodeEntry seeks to: keys at
// keyOff+tableSpace.Len+56 and values at BLOCK_SIZE-valOff (flags&1 == 0).
func putOMapLeaf(blocks map[uint64][]byte, addr, oid, xid uint64, maps []omapMapping) {
	sort.Slice(maps, func(i, j int) bool {
		if maps[i].oid != maps[j].oid {
			return maps[i].oid < maps[j].oid
		}
		return maps[i].xid < maps[j].xid
	})
	buf := make([]byte, types.BLOCK_SIZE)
	writeObjHeader(buf, types.ObjPhysT{Oid: types.OidT(oid), Xid: types.XidT(xid), Type: types.OBJECT_TYPE_BTREE, Subtype: types.OBJECT_TYPE_OMAP})

	n := len(maps)
	binary.LittleEndian.PutUint16(buf[32:34], uint16(types.BTNODE_LEAF|types.BTNODE_FIXED_KV_SIZE))
	binary.LittleEndian.PutUint32(buf[36:40], uint32(n))
	tableLen := uint64(4 * n)
	binary.LittleEndian.PutUint16(buf[42:44], uint16(tableLen)) // TableSpace.Len

	for i, m := range maps {
		keyOff := uint16(16 * i)
		valOff := uint16(16 * (n - i)) // values grow backwards from block end
		toc := 56 + uint64(4*i)
		binary.LittleEndian.PutUint16(buf[toc:toc+2], keyOff)
		binary.LittleEndian.PutUint16(buf[toc+2:toc+4], valOff)

		kpos := uint64(keyOff) + tableLen + 56
		binary.LittleEndian.PutUint64(buf[kpos:kpos+8], m.oid)
		binary.LittleEndian.PutUint64(buf[kpos+8:kpos+16], m.xid)

		vpos := types.BLOCK_SIZE - uint64(valOff)
		binary.LittleEndian.PutUint64(buf[vpos+8:vpos+16], m.paddr)
	}
	blocks[addr] = sealBlock(buf)
}

// putOMap emits an omap_phys_t whose tree points at treeAddr.
func putOMap(blocks map[uint64][]byte, addr, treeAddr uint64) {
	buf := make([]byte, types.BLOCK_SIZE)
	writeObjHeader(buf, types.ObjPhysT{Type: types.OBJECT_TYPE_OMAP})
	binary.LittleEndian.PutUint32(buf[32+8:32+12], uint32(types.OBJECT_TYPE_BTREE)) // TreeType
	binary.LittleEndian.PutUint64(buf[32+16:32+24], treeAddr)                       // TreeOid
	blocks[addr] = sealBlock(buf)
}

// putVolumeSB emits an APFS volume superblock chained to an omap at omapAddr.
func putVolumeSB(blocks map[uint64][]byte, addr, oid, xid uint64, name string, uuid types.UUID, role types.VolRole, omapAddr uint64) {
	var sb types.ApfsSuperblockT
	sb.OmapOid = types.OidT(omapAddr)
	sb.RootTreeOid = types.OidT(2) // FSROOT_OID, identical in every volume
	sb.VolumeUUID = uuid
	copy(sb.VolumeName[:], name)
	sb.Role = role

	var body bytes.Buffer
	if err := binary.Write(&body, binary.LittleEndian, &sb); err != nil {
		panic(err)
	}
	buf := make([]byte, types.BLOCK_SIZE)
	writeObjHeader(buf, types.ObjPhysT{Oid: types.OidT(oid), Xid: types.XidT(xid), Type: types.OBJECT_TYPE_FS})
	copy(buf[32:], body.Bytes())
	copy(buf[32:36], types.APFS_MAGIC) // magic is an unexported type
	blocks[addr] = sealBlock(buf)
}

func mustUUID(t *testing.T, s string) types.UUID {
	t.Helper()
	u, ok := parseUUID(s)
	if !ok {
		t.Fatalf("bad test uuid %q", s)
	}
	return u
}

type fakeVolume struct {
	oid  uint64
	name string
	uuid string
	role types.VolRole
}

// buildFakeContainer lays out volume chains and an in-memory container omap.
// Layout per volume: sb = base, omap = base+1, omap tree = base+2, root = base+3
// extra appends non-volume/corrupt filesystem OIDs in slice order, keeping
// nx_fs_oid_list traversal deterministic.
func buildFakeContainer(t *testing.T, vols []fakeVolume, extra []omapMapping) (*APFS, *fakeDevice) {
	t.Helper()
	dev := &fakeDevice{blocks: make(map[uint64][]byte)}
	containerEntries := make(map[types.OidT]uint64)
	fsOids := make([]types.OidT, 0, types.NX_MAX_FILE_SYSTEMS)

	const xid = 100
	for i, v := range vols {
		base := uint64(0x100 + i*0x100)
		omapAddr, treeAddr, rootAddr := base+1, base+2, base+3
		putFSRoot(dev.blocks, rootAddr, 2, xid)
		putOMapLeaf(dev.blocks, treeAddr, 900+uint64(i), xid, []omapMapping{{oid: 2, xid: xid, paddr: rootAddr}})
		putOMap(dev.blocks, omapAddr, treeAddr)
		putVolumeSB(dev.blocks, base, v.oid, xid, v.name, mustUUID(t, v.uuid), v.role, omapAddr)
		containerEntries[types.OidT(v.oid)] = base
		fsOids = append(fsOids, types.OidT(v.oid))
	}
	for _, e := range extra {
		containerEntries[types.OidT(e.oid)] = e.paddr
		fsOids = append(fsOids, types.OidT(e.oid))
	}

	a := &APFS{r: dev, dev: disk.NewGeneric(dev)}

	tree := types.BTreeNodePhys{}
	tree.Flags = types.BTNODE_LEAF | types.BTNODE_FIXED_KV_SIZE
	keys := make([]types.OidT, 0, len(containerEntries))
	for oid := range containerEntries {
		keys = append(keys, oid)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	tree.Nkeys = uint32(len(keys))
	for _, oid := range keys {
		tree.Entries = append(tree.Entries, types.OMapNodeEntry{
			Key: types.OMapKey{Oid: oid, Xid: xid},
			Val: types.OMapVal{Paddr: containerEntries[oid]},
		})
	}

	var nx types.NxSuperblock
	copy(nx.FsOids[:], fsOids)
	nx.OMap = &types.Obj{Body: types.OMap{Tree: &types.Obj{Body: tree}}}
	a.Valid = &nx
	return a, dev
}

func fixtureVolumes() []fakeVolume {
	return []fakeVolume{
		{oid: 501, name: "System", uuid: "11111111-1111-1111-1111-111111111111", role: types.APFS_VOL_ROLE_SYSTEM},
		{oid: 502, name: "Data", uuid: "22222222-2222-2222-2222-222222222222", role: types.APFS_VOL_ROLE_DATA},
		{oid: 503, name: "System 2", uuid: "33333333-3333-3333-3333-333333333333", role: types.APFS_VOL_ROLE_SYSTEM},
		{oid: 504, name: "Data", uuid: "44444444-4444-4444-4444-444444444444", role: types.APFS_VOL_ROLE_NONE},
	}
}

func putReaper(blocks map[uint64][]byte, addr, oid uint64) {
	buf := make([]byte, types.BLOCK_SIZE)
	writeObjHeader(buf, types.ObjPhysT{Oid: types.OidT(oid), Type: types.OBJECT_TYPE_NX_REAPER})
	blocks[addr] = sealBlock(buf)
}

func TestDiscoverVolumes(t *testing.T) {
	a, dev := buildFakeContainer(t, fixtureVolumes(), []omapMapping{
		{oid: 505, paddr: 0x500}, // checksum-valid non-volume object
		{oid: 506, paddr: 0x600}, // no block on disk -> checksum failure
	})
	// checksum-valid non-volume object (a reaper) at 0x500
	putReaper(dev.blocks, 0x500, 505)
	// append oid 507 to nx_fs_oid_list without any container omap entry
	a.Valid.FsOids[6] = 507

	if err := a.discoverVolumes(); err != nil {
		t.Fatalf("discoverVolumes: %v", err)
	}
	vols := a.Volumes()
	if got, want := len(vols), 7; got != want {
		t.Fatalf("discovered %d volumes, want %d", got, want)
	}

	// stable ordinal indexes
	for i, v := range vols {
		if v.Index != i {
			t.Fatalf("volume %d has index %d", i, v.Index)
		}
	}
	// second call must return an independent, identical enumeration
	again := a.Volumes()
	again[0].Name = "mutated"
	if a.Volumes()[0].Name != "System" {
		t.Fatal("Volumes() must return a copy")
	}

	sys := vols[0]
	if sys.Name != "System" {
		t.Fatalf("name = %q", sys.Name)
	}
	if sys.UUID != mustUUID(t, "11111111-1111-1111-1111-111111111111") {
		t.Fatalf("uuid = %s", sys.UUID)
	}
	if sys.Role != types.APFS_VOL_ROLE_SYSTEM || sys.Role.String() != "System" {
		t.Fatalf("role = %s", sys.Role)
	}
	if !sys.Mountable || sys.paddr != 0x100 {
		t.Fatalf("system volume: %+v", sys)
	}

	// trailing NUL bytes never leak into names
	if strings.ContainsAny(vols[1].Name, "\x00") {
		t.Fatalf("name contains NUL bytes: %q", vols[1].Name)
	}

	reaper := vols[4]
	if reaper.Mountable || !reaper.notVolume {
		t.Fatalf("reaper entry = %+v", reaper)
	}
	if !strings.Contains(reaper.Issue, "REAPER") {
		t.Fatalf("reaper issue = %q, want object type", reaper.Issue)
	}

	corrupt := vols[5]
	if corrupt.Mountable || corrupt.notVolume || !strings.Contains(corrupt.Issue, "checksum") {
		t.Fatalf("corrupt entry = %+v", corrupt)
	}

	missing := vols[6]
	if missing.Mountable || !strings.Contains(missing.Issue, "no entry") {
		t.Fatalf("missing-entry = %+v", missing)
	}

	// table rendering surfaces every identity
	rendered := FormatVolumes(vols)
	for _, want := range []string{"INDEX", "System", "Data", "System 2", "11111111", "unavailable"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("rendering missing %q:\n%s", want, rendered)
		}
	}
}

func TestSelectVolumeSuccess(t *testing.T) {
	a, _ := buildFakeContainer(t, fixtureVolumes(), nil)
	if err := a.discoverVolumes(); err != nil {
		t.Fatal(err)
	}
	if err := a.SelectVolumeByIndex(0); err != nil {
		t.Fatalf("SelectVolumeByIndex: %v", err)
	}
	if sel := a.SelectedVolume(); sel == nil || sel.Index != 0 || sel.Name != "System" {
		t.Fatalf("selected = %+v", sel)
	}
	if a.Volume == nil || a.Volume.Role != types.APFS_VOL_ROLE_SYSTEM {
		t.Fatal("volume superblock not active after selection")
	}
	if a.fsOMapBtree == nil {
		t.Fatal("omap root not active")
	}
	if !a.FSRootBtree.IsRoot() {
		t.Fatal("fs root not active")
	}

	if err := a.SelectVolumeByName("System 2"); err != nil {
		t.Fatalf("SelectVolumeByName: %v", err)
	}
	if a.SelectedVolume().Index != 2 {
		t.Fatalf("unique name selected %+v", a.SelectedVolume())
	}
	// two volumes are named "Data" -> name must be ambiguous
	if err := a.SelectVolumeByName("Data"); !errors.Is(err, ErrVolumeAmbiguous) {
		t.Fatalf("duplicate name error = %v, want ErrVolumeAmbiguous", err)
	}
	if err := a.SelectVolumeByName("nope"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("missing name error = %v, want ErrVolumeNotFound", err)
	}

	if err := a.SelectVolumeByUUID("44444444444444444444444444444444"); err != nil {
		t.Fatalf("bare uuid selection: %v", err)
	}
	if a.SelectedVolume().Index != 3 {
		t.Fatalf("uuid selected %+v", a.SelectedVolume())
	}
	if err := a.SelectVolumeByUUID("not-a-uuid"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("bad uuid error = %v", err)
	}

	// role System is shared by indexes 0 and 2
	if err := a.SelectVolumeByRole("system"); !errors.Is(err, ErrVolumeAmbiguous) {
		t.Fatalf("role ambiguity error = %v", err)
	}
	// role Data is unique
	if err := a.SelectVolumeByRole("data"); err != nil {
		t.Fatalf("role select: %v", err)
	}
	if a.SelectedVolume().Index != 1 {
		t.Fatalf("role selected %+v", a.SelectedVolume())
	}
	if err := a.SelectVolumeByRole("preboot"); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("missing role error = %v", err)
	}
}

func TestSelectVolumeGenericSelector(t *testing.T) {
	a, _ := buildFakeContainer(t, fixtureVolumes(), nil)
	if err := a.discoverVolumes(); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		selector string
		index    int
	}{
		{"2", 2},       // decimal index
		{"index:3", 3}, // forced index
		{"33333333-3333-3333-3333-333333333333", 2}, // uuid
		{"uuid:22222222-2222-2222-2222-222222222222", 1},
		{"role:none", 3},   // forced role
		{"name:System", 0}, // forced name even though System is a role keyword
		{"System 2", 2},    // bare name
	} {
		t.Run(tc.selector, func(t *testing.T) {
			if err := a.SelectVolume(tc.selector); err != nil {
				t.Fatalf("SelectVolume(%q): %v", tc.selector, err)
			}
			if got := a.SelectedVolume().Index; got != tc.index {
				t.Fatalf("SelectVolume(%q) selected index %d, want %d", tc.selector, got, tc.index)
			}
		})
	}

	// "System" bare resolves as role and is ambiguous
	err := a.SelectVolume("System")
	if !errors.Is(err, ErrVolumeAmbiguous) {
		t.Fatalf("bare System error = %v, want role ambiguity", err)
	}
	var amb *AmbiguousVolumeError
	if !errors.As(err, &amb) || amb.Kind != "role" || len(amb.Matches) != 2 {
		t.Fatalf("ambiguity = %#v", err)
	}
	// message carries both indexes for disambiguation
	for _, idx := range []int{0, 2} {
		if !strings.Contains(err.Error(), fmt.Sprintf("%d", idx)) {
			t.Fatalf("ambiguity message missing index %d: %v", idx, err)
		}
	}
}

func TestSelectNonVolumeAndBrokenObjects(t *testing.T) {
	a, dev := buildFakeContainer(t, fixtureVolumes(), []omapMapping{
		{oid: 505, paddr: 0x500},
		{oid: 506, paddr: 0x600}, // missing block -> checksum failure
	})
	putReaper(dev.blocks, 0x500, 505)
	if err := a.discoverVolumes(); err != nil {
		t.Fatal(err)
	}

	err := a.SelectVolumeByIndex(4)
	if !errors.Is(err, ErrNotAVolume) {
		t.Fatalf("reaper select error = %v, want ErrNotAVolume", err)
	}
	var nav *NotAVolumeError
	if !errors.As(err, &nav) || nav.OID != 505 || nav.Paddr != 0x500 {
		t.Fatalf("not-a-volume error = %#v", err)
	}

	err = a.SelectVolumeByIndex(5)
	if !errors.Is(err, ErrVolumeNotMountable) {
		t.Fatalf("broken select error = %v, want ErrVolumeNotMountable", err)
	}
	if !errors.Is(err, types.ErrBadBlockChecksum) {
		t.Fatalf("broken select should wrap checksum error: %v", err)
	}

	if err := a.SelectVolumeByIndex(99); !errors.Is(err, ErrVolumeNotFound) {
		t.Fatalf("bad index error = %v", err)
	}
}

func TestFailedSelectionKeepsPreviousVolume(t *testing.T) {
	a, dev := buildFakeContainer(t, fixtureVolumes(), []omapMapping{
		{oid: 505, paddr: 0x500},
		{oid: 506, paddr: 0x600},
	})
	putReaper(dev.blocks, 0x500, 505)
	if err := a.discoverVolumes(); err != nil {
		t.Fatal(err)
	}

	if err := a.SelectVolumeByIndex(0); err != nil {
		t.Fatal(err)
	}
	prevOMap := a.fsOMapBtree
	prevOID := a.volume.Hdr.Oid

	for _, bad := range []int{4, 5} {
		if err := a.SelectVolumeByIndex(bad); err == nil {
			t.Fatalf("selecting broken index %d succeeded", bad)
		}
		if a.fsOMapBtree != prevOMap || !a.fsOMapBtree.NodeCacheEnabled() {
			t.Fatalf("failed selection of %d mutated active volume state", bad)
		}
		if a.volume.Hdr.Oid != prevOID || a.SelectedVolume().Index != 0 {
			t.Fatalf("failed selection of %d changed active volume", bad)
		}
	}

	// other valid volumes remain listable and selectable after failures
	if err := a.SelectVolumeByIndex(1); err != nil {
		t.Fatalf("re-select after failure: %v", err)
	}
	if a.volume.Hdr.Oid != 502 || a.SelectedVolume().Index != 1 {
		t.Fatalf("active volume = oid %#x index %d", a.volume.Hdr.Oid, a.SelectedVolume().Index)
	}
}

func TestReSelectionResetsRootAndCache(t *testing.T) {
	a, _ := buildFakeContainer(t, fixtureVolumes(), nil)
	if err := a.discoverVolumes(); err != nil {
		t.Fatal(err)
	}

	if err := a.SelectVolumeByIndex(0); err != nil {
		t.Fatal(err)
	}
	sysRoot, err := a.fsOMapBtree.GetOMapEntry(a.r, 2, maxXid)
	if err != nil {
		t.Fatal(err)
	}
	if sysRoot.Val.Paddr != 0x103 {
		t.Fatalf("system fsroot paddr = %#x", sysRoot.Val.Paddr)
	}
	oldOMap := a.fsOMapBtree
	if !oldOMap.NodeCacheEnabled() {
		t.Fatal("initial selection did not enable node cache")
	}

	if err := a.SelectVolumeByIndex(1); err != nil {
		t.Fatal(err)
	}
	if a.fsOMapBtree == oldOMap {
		t.Fatal("omap root pointer reused across volume selection")
	}
	// The omap root is freshly decoded from disk for every attempt, so its
	// cache starts nil and the enabled cache here can only be a brand-new one
	// that cannot hold nodes cached for the previous volume.
	if !a.fsOMapBtree.NodeCacheEnabled() {
		t.Fatal("fresh node cache not installed on re-selection")
	}
	// both volumes reuse RootTreeOid 2; it must resolve to the new volume's root
	dataRoot, err := a.fsOMapBtree.GetOMapEntry(a.r, 2, maxXid)
	if err != nil {
		t.Fatal(err)
	}
	if dataRoot.Val.Paddr != 0x203 {
		t.Fatalf("data fsroot paddr = %#x, stale system root leaked across volumes", dataRoot.Val.Paddr)
	}

	// switch back
	if err := a.SelectVolumeByIndex(0); err != nil {
		t.Fatal(err)
	}
	again, err := a.fsOMapBtree.GetOMapEntry(a.r, 2, maxXid)
	if err != nil {
		t.Fatal(err)
	}
	if again.Val.Paddr != 0x103 {
		t.Fatalf("system fsroot after switching back = %#x", again.Val.Paddr)
	}
}

func TestOperationsRequireSelection(t *testing.T) {
	a, _ := buildFakeContainer(t, fixtureVolumes(), nil)
	// discover but do not select: simulates a multi-volume open()
	if err := a.discoverVolumes(); err != nil {
		t.Fatal(err)
	}
	if a.SelectedVolume() != nil {
		t.Fatal("no volume should be active")
	}

	check := func(name string, fn func() error) {
		t.Helper()
		err := fn()
		if !errors.Is(err, ErrVolumeSelectionRequired) {
			t.Fatalf("%s error = %v, want ErrVolumeSelectionRequired", name, err)
		}
		if !strings.Contains(err.Error(), "System") || !strings.Contains(err.Error(), "Data") {
			t.Fatalf("%s error must list available identities: %v", name, err)
		}
	}
	check("List", func() error { return a.List("/") })
	check("Cat", func() error { return a.Cat("/x") })
	check("Copy", func() error { return a.Copy("/x", t.TempDir()) })
	check("OidInfo", func() error { return a.OidInfo(2) })
	check("Tree", func() error { return a.Tree("/") })
}

func TestAutoSelectVolume(t *testing.T) {
	// multiple mountable volumes: nothing selected
	multi, _ := buildFakeContainer(t, fixtureVolumes(), nil)
	if err := multi.discoverVolumes(); err != nil {
		t.Fatal(err)
	}
	if err := multi.autoSelectVolume(); err != nil {
		t.Fatalf("auto-select multi: %v", err)
	}
	if multi.fsOMapBtree != nil || multi.SelectedVolume() != nil {
		t.Fatal("multi-volume container must not auto-select")
	}

	// single mountable volume: activated, preserving the default behaviour
	single, _ := buildFakeContainer(t, fixtureVolumes()[:1], nil)
	if err := single.discoverVolumes(); err != nil {
		t.Fatal(err)
	}
	if err := single.autoSelectVolume(); err != nil {
		t.Fatalf("auto-select single: %v", err)
	}
	if single.SelectedVolume() == nil || single.SelectedVolume().Index != 0 {
		t.Fatal("single mountable volume must be auto-selected")
	}

	// no mountable volumes: error
	none, _ := buildFakeContainer(t, nil, nil)
	none.Valid.FsOids[0] = 999 // oid absent from container omap
	if err := none.discoverVolumes(); err != nil {
		t.Fatal(err)
	}
	if err := none.autoSelectVolume(); err == nil || !strings.Contains(err.Error(), "no mountable") {
		t.Fatalf("auto-select empty = %v", err)
	}
}

func TestParseVolumeSelector(t *testing.T) {
	for _, tc := range []struct {
		in          string
		kind, value string
	}{
		{"0", "index", "0"},
		{"12", "index", "12"},
		{"  index: 7 ", "index", "7"},
		{"name:System", "name", "System"},
		{"uuid:22222222-2222-2222-2222-222222222222", "uuid", "22222222-2222-2222-2222-222222222222"},
		{"role:data", "role", "data"},
		{"22222222-2222-2222-2222-222222222222", "uuid", "22222222-2222-2222-2222-222222222222"},
		{"Preboot", "role", "Preboot"},
		{"Macintosh HD", "name", "Macintosh HD"},
		{"weird0", "name", "weird0"},
	} {
		kind, value := parseVolumeSelector(tc.in)
		if kind != tc.kind || value != tc.value {
			t.Fatalf("parseVolumeSelector(%q) = %s:%q, want %s:%q", tc.in, kind, value, tc.kind, tc.value)
		}
	}
}

func TestTrimVolumeName(t *testing.T) {
	if got := trimVolumeName([]byte("Macintosh HD\x00\x00\x00")); got != "Macintosh HD" {
		t.Fatalf("trim = %q", got)
	}
	// internal NUL bytes are preserved; only trailing padding is removed
	if got := trimVolumeName([]byte("a\x00b\x00\x00")); got != "a\x00b" {
		t.Fatalf("trim = %q", got)
	}
}

func TestFormatVolumesStable(t *testing.T) {
	a, _ := buildFakeContainer(t, fixtureVolumes(), nil)
	if err := a.discoverVolumes(); err != nil {
		t.Fatal(err)
	}
	out := FormatVolumes(a.Volumes())
	wantRows := [][]string{
		{"INDEX", "NAME", "UUID", "ROLE"},
		{"0", "System", "11111111-1111-1111-1111-111111111111", "System"},
		{"1", "Data", "22222222-2222-2222-2222-222222222222", "Data"},
		{"2", "System 2", "33333333-3333-3333-3333-333333333333", "System"},
		{"3", "Data", "44444444-4444-4444-4444-444444444444", "None"},
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != len(wantRows) {
		t.Fatalf("FormatVolumes has %d rows, want %d:\n%s", len(lines), len(wantRows), out)
	}
	for i, want := range wantRows {
		got := strings.Fields(lines[i])
		if len(got) < 4 {
			t.Fatalf("row %d = %v, want at least 4 columns", i, got)
		}
		// columns are: index, name (may contain spaces), uuid, role
		index, role := got[0], got[len(got)-1]
		uuid := got[len(got)-2]
		name := strings.Join(got[1:len(got)-2], " ")
		if index != want[0] || name != want[1] || uuid != want[2] || role != want[3] {
			t.Fatalf("row %d = [%s | %s | %s | %s], want %v\n%s", i, index, name, uuid, role, want, out)
		}
	}
}
