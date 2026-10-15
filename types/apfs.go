package types

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type volFlag uint64

// VolRole is the role assigned to an APFS volume (apfs_role_t).
type VolRole uint16
type optVolFeatFlag uint64
type incompatVolFeatFlag uint64

const (
	/** Volume Flags **/
	APFS_FS_UNENCRYPTED            volFlag = 0x00000001
	APFS_FS_RESERVED_2             volFlag = 0x00000002
	APFS_FS_RESERVED_4             volFlag = 0x00000004
	APFS_FS_ONEKEY                 volFlag = 0x00000008
	APFS_FS_SPILLEDOVER            volFlag = 0x00000010
	APFS_FS_RUN_SPILLOVER_CLEANER  volFlag = 0x00000020
	APFS_FS_ALWAYS_CHECK_EXTENTREF volFlag = 0x00000040
	APFS_FS_RESERVED_80            volFlag = 0x00000080
	APFS_FS_RESERVED_100           volFlag = 0x00000100

	APFS_FS_FLAGS_VALID_MASK = (APFS_FS_UNENCRYPTED | APFS_FS_RESERVED_2 | APFS_FS_RESERVED_4 | APFS_FS_ONEKEY | APFS_FS_SPILLEDOVER | APFS_FS_RUN_SPILLOVER_CLEANER | APFS_FS_ALWAYS_CHECK_EXTENTREF | APFS_FS_RESERVED_80 | APFS_FS_RESERVED_100)

	APFS_FS_CRYPTOFLAGS = (APFS_FS_UNENCRYPTED | APFS_FS_ONEKEY)

	/** Volume Roles **/
	APFS_VOL_ROLE_NONE VolRole = 0x0000

	APFS_VOL_ROLE_SYSTEM    VolRole = 0x0001
	APFS_VOL_ROLE_USER      VolRole = 0x0002
	APFS_VOL_ROLE_RECOVERY  VolRole = 0x0004
	APFS_VOL_ROLE_VM        VolRole = 0x0008
	APFS_VOL_ROLE_PREBOOT   VolRole = 0x0010
	APFS_VOL_ROLE_INSTALLER VolRole = 0x0020

	APFS_VOLUME_ENUM_SHIFT = 6

	APFS_VOL_ROLE_DATA     VolRole = (1 << APFS_VOLUME_ENUM_SHIFT) // = 0x0040 --- formerly defined explicitly as `0x0040`
	APFS_VOL_ROLE_BASEBAND VolRole = (2 << APFS_VOLUME_ENUM_SHIFT) // = 0x0080 --- formerly defined explicitly as `0x0080`

	// Roles supported since revision 2020-05-15 --- macOS 10.15+, iOS 13+
	APFS_VOL_ROLE_UPDATE      VolRole = (3 << APFS_VOLUME_ENUM_SHIFT)  // = 0x00c0
	APFS_VOL_ROLE_XART        VolRole = (4 << APFS_VOLUME_ENUM_SHIFT)  // = 0x0100
	APFS_VOL_ROLE_HARDWARE    VolRole = (5 << APFS_VOLUME_ENUM_SHIFT)  // = 0x0140
	APFS_VOL_ROLE_BACKUP      VolRole = (6 << APFS_VOLUME_ENUM_SHIFT)  // = 0x0180
	APFS_VOL_ROLE_RESERVED_7  VolRole = (7 << APFS_VOLUME_ENUM_SHIFT)  // = 0x01c0 --- spec also uses the name `APFS_VOL_ROLE_SIDECAR`, but that could be an error
	APFS_VOL_ROLE_RESERVED_8  VolRole = (8 << APFS_VOLUME_ENUM_SHIFT)  // = 0x0200 --- formerly named `APFS_VOL_ROLE_RESERVED_200`
	APFS_VOL_ROLE_ENTERPRISE  VolRole = (9 << APFS_VOLUME_ENUM_SHIFT)  // = 0x0240
	APFS_VOL_ROLE_RESERVED_10 VolRole = (10 << APFS_VOLUME_ENUM_SHIFT) // = 0x0280
	APFS_VOL_ROLE_PRELOGIN    VolRole = (11 << APFS_VOLUME_ENUM_SHIFT) // = 0x02c0

	/** Optional Volume Feature Flags **/
	APFS_FEATURE_DEFRAG_PRERELEASE       optVolFeatFlag = 0x00000001
	APFS_FEATURE_HARDLINK_MAP_RECORDS    optVolFeatFlag = 0x00000002
	APFS_FEATURE_DEFRAG                  optVolFeatFlag = 0x00000004
	APFS_FEATURE_STRICTATIME             optVolFeatFlag = 0x00000008
	APFS_FEATURE_VOLGRP_SYSTEM_INO_SPACE optVolFeatFlag = 0x00000010

	APFS_SUPPORTED_FEATURES_MASK = (APFS_FEATURE_DEFRAG | APFS_FEATURE_DEFRAG_PRERELEASE | APFS_FEATURE_HARDLINK_MAP_RECORDS | APFS_FEATURE_STRICTATIME | APFS_FEATURE_VOLGRP_SYSTEM_INO_SPACE)

	/** Read-Only Comaptible Volume Feature Flags **/
	APFS_SUPPORTED_ROCOMPAT_MASK = 0

	/** Incompatible Volume Feature Flags **/
	APFS_INCOMPAT_CASE_INSENSITIVE          incompatVolFeatFlag = 0x00000001
	APFS_INCOMPAT_DATALESS_SNAPS            incompatVolFeatFlag = 0x00000002
	APFS_INCOMPAT_ENC_ROLLED                incompatVolFeatFlag = 0x00000004
	APFS_INCOMPAT_NORMALIZATION_INSENSITIVE incompatVolFeatFlag = 0x00000008
	APFS_INCOMPAT_INCOMPLETE_RESTORE        incompatVolFeatFlag = 0x00000010
	APFS_INCOMPAT_SEALED_VOLUME             incompatVolFeatFlag = 0x00000020
	APFS_INCOMPAT_RESERVED_40               incompatVolFeatFlag = 0x00000040

	APFS_SUPPORTED_INCOMPAT_MASK = (APFS_INCOMPAT_CASE_INSENSITIVE | APFS_INCOMPAT_DATALESS_SNAPS | APFS_INCOMPAT_ENC_ROLLED | APFS_INCOMPAT_NORMALIZATION_INSENSITIVE | APFS_INCOMPAT_INCOMPLETE_RESTORE | APFS_INCOMPAT_SEALED_VOLUME | APFS_INCOMPAT_RESERVED_40)
)

const APFS_MODIFIED_NAMELEN = 32

type EpochTime uint64

func (e EpochTime) String() string {
	t := time.Unix(0, int64(e))
	// return t.Format(time.UnixDate)
	return timeColor(t.Format("01Jan06 15:04:05"))
}

type apfs_modified_by_t struct {
	ID        [APFS_MODIFIED_NAMELEN]byte
	Timestamp EpochTime
	LastXid   XidT
}

const (
	APFS_MAGIC       = "APSB"
	APFS_MAX_HIST    = 8
	APFS_VOLNAME_LEN = 256
)

// ApfsSuperblockT is a apfs_superblock_t struct
type ApfsSuperblockT struct {
	// Obj ObjPhysT

	Magic   magic
	FsIndex uint32

	Features                   optVolFeatFlag
	ReadonlyCompatibleFeatures uint64
	IncompatibleFeatures       incompatVolFeatFlag

	UnmountTime EpochTime

	FsReserveBlockCount uint64
	FsQuotaBlockCount   uint64
	FsAllocCount        uint64

	MetaCrypto wrapped_meta_crypto_state_t

	RootTreeType      objType
	ExtentRefTreeType objType
	SnapMetaTreeType  objType

	OmapOid          OidT
	RootTreeOid      OidT
	ExtentrefTreeOid OidT
	SnapMetaTreeOid  OidT

	RevertToXid       XidT
	RevertToSblockOid OidT

	NextObjID uint64

	NumFiles          uint64
	NumDirectories    uint64
	NumSymlinks       uint64
	NumOtherFsobjects uint64
	NumSnapshots      uint64

	TotalBlockAlloced uint64
	TotalBlocksFreed  uint64

	VolumeUUID  UUID
	LastModTime EpochTime

	FsFlags volFlag

	FormattedBy apfs_modified_by_t
	ModifiedBy  [APFS_MAX_HIST]apfs_modified_by_t

	VolumeName [APFS_VOLNAME_LEN]byte
	NextDocID  uint32

	Role     VolRole
	Reserved uint16

	RootToXid  XidT
	ErStateOid OidT

	/* Fields introduced in revision 2020-05-15 */

	// Fields supported on macOS 10.13.3+
	CloneinfoIDEpoch EpochTime
	CloneinfoXid     uint64

	// Fields supported on macOS 10.15+
	SnapMetaExtOid OidT
	VolumeGroupID  UUID

	/* Fields introduced in revision 2020-06-22 */

	// Fields supported on macOS 11+
	IntegrityMetaOid OidT
	FextTreeOid      OidT
	FextTreeType     objType

	ReservedType uint32
	ReservedOid  OidT
}

type ApfsSuperblock struct {
	ApfsSuperblockT

	OMap          *Obj
	ExtentRefTree *Obj
	SnapMetaTree  *Obj

	block
}

// String returns the canonical name of a volume role, or role(0xNNN) for an
// unrecognised value.
func (r VolRole) String() string {
	switch r {
	case APFS_VOL_ROLE_NONE:
		return "None"
	case APFS_VOL_ROLE_SYSTEM:
		return "System"
	case APFS_VOL_ROLE_USER:
		return "User"
	case APFS_VOL_ROLE_RECOVERY:
		return "Recovery"
	case APFS_VOL_ROLE_VM:
		return "VM"
	case APFS_VOL_ROLE_PREBOOT:
		return "Preboot"
	case APFS_VOL_ROLE_INSTALLER:
		return "Installer"
	case APFS_VOL_ROLE_DATA:
		return "Data"
	case APFS_VOL_ROLE_BASEBAND:
		return "Baseband"
	case APFS_VOL_ROLE_UPDATE:
		return "Update"
	case APFS_VOL_ROLE_XART:
		return "XART"
	case APFS_VOL_ROLE_HARDWARE:
		return "Hardware"
	case APFS_VOL_ROLE_BACKUP:
		return "Backup"
	case APFS_VOL_ROLE_ENTERPRISE:
		return "Enterprise"
	case APFS_VOL_ROLE_PRELOGIN:
		return "Prelogin"
	default:
		return fmt.Sprintf("role(%#x)", uint16(r))
	}
}

// ParseVolRole parses a case-insensitive role name (e.g. "System", "data",
// "PREBOOT") or a hexadecimal value (e.g. "0x10") into a VolRole.
func ParseVolRole(s string) (VolRole, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "none":
		return APFS_VOL_ROLE_NONE, true
	case "system":
		return APFS_VOL_ROLE_SYSTEM, true
	case "user":
		return APFS_VOL_ROLE_USER, true
	case "recovery":
		return APFS_VOL_ROLE_RECOVERY, true
	case "vm":
		return APFS_VOL_ROLE_VM, true
	case "preboot":
		return APFS_VOL_ROLE_PREBOOT, true
	case "installer":
		return APFS_VOL_ROLE_INSTALLER, true
	case "data":
		return APFS_VOL_ROLE_DATA, true
	case "baseband":
		return APFS_VOL_ROLE_BASEBAND, true
	case "update":
		return APFS_VOL_ROLE_UPDATE, true
	case "xart":
		return APFS_VOL_ROLE_XART, true
	case "hardware":
		return APFS_VOL_ROLE_HARDWARE, true
	case "backup":
		return APFS_VOL_ROLE_BACKUP, true
	case "enterprise":
		return APFS_VOL_ROLE_ENTERPRISE, true
	case "prelogin":
		return APFS_VOL_ROLE_PRELOGIN, true
	}
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(strings.ToLower(trimmed), "0x") {
		if v, err := strconv.ParseUint(trimmed[2:], 16, 16); err == nil {
			return VolRole(v), true
		}
	}
	return 0, false
}
