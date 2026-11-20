/* Real Windows structs filled with known values at compile time. The bytes
   are pulled out of the object file (objcopy) and parsed by the Go tests, so
   the parsers are checked against genuine SDK layouts without running Windows.
   Build: see tools/gen_testdata.sh */
#include <windows.h>
#include <winioctl.h>
#include <stddef.h>

#define SEC(n) __attribute__((section(n), used))
#define EFI   {0xC12A7328, 0xF81F, 0x11D2, {0xBA,0x4B,0x00,0xA0,0xC9,0x3E,0xC9,0x3B}}
#define BASIC {0xEBD0A0A2, 0xB9E5, 0x4433, {0x87,0xC0,0x68,0xB6,0xB7,0x26,0x99,0xC7}}

struct gpt3 { DRIVE_LAYOUT_INFORMATION_EX h; PARTITION_INFORMATION_EX more[2]; };
_Static_assert(offsetof(struct gpt3, more) == 48 + 144, "contiguous");
SEC(".l_gpt") struct gpt3 layout_gpt = {
    .h = { .PartitionStyle = PARTITION_STYLE_GPT, .PartitionCount = 3,
           .PartitionEntry = {{ .PartitionStyle = PARTITION_STYLE_GPT,
               .StartingOffset = {.QuadPart = 1048576}, .PartitionLength = {.QuadPart = 104857600},
               .PartitionNumber = 1, .Gpt = { .PartitionType = EFI, .Name = L"EFI system partition" } }} },
    .more = {
        { 0 }, /* unused slot (number 0) must be skipped */
        { .PartitionStyle = PARTITION_STYLE_GPT, .StartingOffset = {.QuadPart = 122683392},
          .PartitionLength = {.QuadPart = 0x123456789A00LL}, .PartitionNumber = 3,
          .Gpt = { .PartitionType = BASIC, .Name = L"Basic data partition" } },
    },
};

struct mbr2 { DRIVE_LAYOUT_INFORMATION_EX h; PARTITION_INFORMATION_EX more[1]; };
SEC(".l_mbr") struct mbr2 layout_mbr = {
    .h = { .PartitionStyle = PARTITION_STYLE_MBR, .PartitionCount = 2, .Mbr = { .Signature = 0xDEADBEEF },
           .PartitionEntry = {{ .PartitionStyle = PARTITION_STYLE_MBR,
               .StartingOffset = {.QuadPart = 4194304}, .PartitionLength = {.QuadPart = 61991813120LL},
               .PartitionNumber = 1, .Mbr = { .PartitionType = 0x0C, .BootIndicator = TRUE } }} },
    .more = {{ .PartitionStyle = PARTITION_STYLE_MBR, .StartingOffset = {.QuadPart = 0},
               .PartitionLength = {.QuadPart = 0}, .PartitionNumber = 0 }},
};

struct ext2 { VOLUME_DISK_EXTENTS h; DISK_EXTENT more[1]; };
SEC(".l_ext") struct ext2 extents = {
    .h = { .NumberOfDiskExtents = 2, .Extents = {{ .DiskNumber = 2,
           .StartingOffset = {.QuadPart = 1048576}, .ExtentLength = {.QuadPart = 5000000000LL} }} },
    .more = {{ .DiskNumber = 7, .StartingOffset = {.QuadPart = 0x100000000LL}, .ExtentLength = {.QuadPart = 4096} }},
};

struct sdd { STORAGE_DEVICE_DESCRIPTOR d; char pad[200]; };
SEC(".l_sdd") struct sdd storage = {
    .d = { .Version = sizeof(STORAGE_DEVICE_DESCRIPTOR), .Size = 200, .RemovableMedia = TRUE,
           .BusType = BusTypeUsb, .VendorIdOffset = 100, .ProductIdOffset = 120 },
};

SEC(".l_geo") DISK_GEOMETRY_EX geometry = {
    .Geometry = { .BytesPerSector = 4096 }, .DiskSize = {.QuadPart = 1000204886016LL},
};
