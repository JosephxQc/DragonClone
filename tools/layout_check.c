/* Compile-time check that the struct offsets and IOCTL codes we hard-code in
   core/layout.go, core/platform_windows.go and launcher_windows.go match the
   Windows SDK headers.
   Build: x86_64-w64-mingw32-gcc -c layout_check.c   (no output = all good) */
#include <windows.h>
#include <winioctl.h>
#include <stddef.h>

#define CHECK(c) _Static_assert(c, #c)

/* layout.go */
CHECK(offsetof(DRIVE_LAYOUT_INFORMATION_EX, PartitionEntry) == 48);
CHECK(sizeof(PARTITION_INFORMATION_EX) == 144);
CHECK(offsetof(DRIVE_LAYOUT_INFORMATION_EX, PartitionStyle) == 0);
CHECK(offsetof(DRIVE_LAYOUT_INFORMATION_EX, PartitionCount) == 4);
CHECK(offsetof(PARTITION_INFORMATION_EX, PartitionStyle) == 0);
CHECK(offsetof(PARTITION_INFORMATION_EX, StartingOffset) == 8);
CHECK(offsetof(PARTITION_INFORMATION_EX, PartitionLength) == 16);
CHECK(offsetof(PARTITION_INFORMATION_EX, PartitionNumber) == 24);
CHECK(offsetof(PARTITION_INFORMATION_EX, Mbr.PartitionType) == 32);
CHECK(offsetof(PARTITION_INFORMATION_EX, Gpt.PartitionType) == 32);
CHECK(offsetof(PARTITION_INFORMATION_EX, Gpt.Name) == 72);
CHECK(sizeof(((PARTITION_INFORMATION_EX*)0)->Gpt.Name) == 72);
CHECK(PARTITION_STYLE_MBR == 0 && PARTITION_STYLE_GPT == 1 && PARTITION_STYLE_RAW == 2);

CHECK(offsetof(VOLUME_DISK_EXTENTS, NumberOfDiskExtents) == 0);
CHECK(offsetof(VOLUME_DISK_EXTENTS, Extents) == 8);
CHECK(sizeof(DISK_EXTENT) == 24);
CHECK(offsetof(DISK_EXTENT, DiskNumber) == 0);
CHECK(offsetof(DISK_EXTENT, StartingOffset) == 8);
CHECK(offsetof(DISK_EXTENT, ExtentLength) == 16);

CHECK(offsetof(STORAGE_DEVICE_DESCRIPTOR, RemovableMedia) == 10);
CHECK(offsetof(STORAGE_DEVICE_DESCRIPTOR, VendorIdOffset) == 12);
CHECK(offsetof(STORAGE_DEVICE_DESCRIPTOR, ProductIdOffset) == 16);
CHECK(offsetof(STORAGE_DEVICE_DESCRIPTOR, SerialNumberOffset) == 24);
CHECK(offsetof(STORAGE_DEVICE_DESCRIPTOR, BusType) == 28);
CHECK(sizeof(STORAGE_PROPERTY_QUERY) == 12);
CHECK(StorageDeviceProperty == 0 && PropertyStandardQuery == 0);
CHECK(BusTypeScsi == 1 && BusTypeAtapi == 2 && BusTypeAta == 3 && BusType1394 == 4 && BusTypeFibre == 6);
CHECK(BusTypeUsb == 7 && BusTypeRAID == 8 && BusTypeiScsi == 9 && BusTypeSas == 10 && BusTypeSata == 11);
CHECK(BusTypeSd == 12 && BusTypeMmc == 13 && BusTypeVirtual == 14 && BusTypeFileBackedVirtual == 15);
CHECK(BusTypeSpaces == 16 && BusTypeNvme == 17);

CHECK(offsetof(DISK_GEOMETRY_EX, Geometry) == 0);
CHECK(offsetof(DISK_GEOMETRY, BytesPerSector) == 20);
CHECK(offsetof(DISK_GEOMETRY_EX, DiskSize) == 24);
CHECK(sizeof(GET_LENGTH_INFORMATION) == 8);

/* platform_windows.go */
CHECK(IOCTL_DISK_GET_LENGTH_INFO == 0x0007405C);
CHECK(IOCTL_DISK_GET_DRIVE_GEOMETRY_EX == 0x000700A0);
CHECK(IOCTL_DISK_GET_DRIVE_LAYOUT_EX == 0x00070050);
CHECK(IOCTL_DISK_UPDATE_PROPERTIES == 0x00070140);
CHECK(IOCTL_STORAGE_QUERY_PROPERTY == 0x002D1400);
CHECK(IOCTL_VOLUME_GET_VOLUME_DISK_EXTENTS == 0x00560000);
CHECK(FSCTL_LOCK_VOLUME == 0x00090018);
CHECK(FSCTL_UNLOCK_VOLUME == 0x0009001C);
CHECK(FSCTL_DISMOUNT_VOLUME == 0x00090020);
CHECK(GENERIC_READ == 0x80000000 && GENERIC_WRITE == 0x40000000);
CHECK(FILE_SHARE_READ == 1 && FILE_SHARE_WRITE == 2 && OPEN_EXISTING == 3);
CHECK(FILE_FLAG_NO_BUFFERING == 0x20000000 && FILE_FLAG_WRITE_THROUGH == 0x80000000);
CHECK(ERROR_NOT_READY == 21 && ERROR_WRITE_PROTECT == 19 && ERROR_CRC == 23 && ERROR_SECTOR_NOT_FOUND == 27);
CHECK(ERROR_INVALID_HANDLE == 6 && ERROR_DEV_NOT_EXIST == 55 && ERROR_INVALID_PARAMETER == 87);
CHECK(ERROR_DISK_FULL == 112 && ERROR_FILE_TOO_LARGE == 223 && ERROR_NO_SUCH_DEVICE == 433);
CHECK(ERROR_MEDIA_CHANGED == 1110 && ERROR_NO_MEDIA_IN_DRIVE == 1112 && ERROR_DEVICE_NOT_CONNECTED == 1167);
CHECK(ERROR_PATH_NOT_FOUND == 3 && ERROR_INSUFFICIENT_BUFFER == 122 && ERROR_MORE_DATA == 234 && ERROR_HANDLE_EOF == 38);
CHECK(ERROR_IO_DEVICE == 1117 && ERROR_READ_FAULT == 30);

/* launcher_windows.go */
#include <shellapi.h>
CHECK(sizeof(SHELLEXECUTEINFOW) == 112);
CHECK(offsetof(SHELLEXECUTEINFOW, fMask) == 4);
CHECK(offsetof(SHELLEXECUTEINFOW, lpVerb) == 16);
CHECK(offsetof(SHELLEXECUTEINFOW, lpParameters) == 32);
CHECK(offsetof(SHELLEXECUTEINFOW, nShow) == 48);
CHECK(offsetof(SHELLEXECUTEINFOW, dwHotKey) == 88);
CHECK(offsetof(SHELLEXECUTEINFOW, hProcess) == 104);
CHECK(SEE_MASK_NOCLOSEPROCESS == 0x40 && ERROR_CANCELLED == 1223);
