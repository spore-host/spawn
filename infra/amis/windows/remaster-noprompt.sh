#!/usr/bin/env bash
# Remaster a Windows install ISO so it boots Setup WITHOUT the interactive
# "Press any key to boot from CD or DVD..." prompt — fully autonomous, no
# keystroke timing during the build.
#
# THE PROBLEM
#   The stock Windows ISO's UEFI El Torito boot image is efisys.bin, which prints
#   "Press any key to boot from CD or DVD..." and waits ~5s. In a headless
#   Packer/qemu build there is no reliable way to land a keypress in that window:
#   OVMF shows the prompt at a late, variable delay, then on timeout falls through
#   to PXE and the UEFI shell. We verified this repeatedly via VNC screenshots —
#   the install never started and the disk image never grew.
#
# THE FIX (surgical, preserves the UDF tree + the >4 GiB install.wim untouched)
#   Microsoft ships a no-prompt boot image — efi/microsoft/boot/efisys_noprompt.bin
#   — on the SAME media, byte-for-byte the same size (1474560 B = 720×2048-byte
#   sectors) as efisys.bin. The ISO's El Torito UEFI entry is an embedded copy of
#   efisys.bin at a fixed LBA. We copy the ISO and overwrite exactly that boot
#   image with efisys_noprompt.bin via dd. Nothing else in the ISO changes, so
#   UDF + the 6.2 GB install.wim are bit-identical; only the boot image loses the
#   prompt. We can't simply rebuild the ISO because this xorriso lacks UDF write
#   support in mkisofs emulation and install.wim exceeds the 4 GiB ISO9660 limit.
#
# Usage: ./remaster-noprompt.sh <input.iso> <output.iso>
# Requires: xorriso (to locate the El Torito boot image LBA), sudo (loop mount to
# read efisys_noprompt.bin), dd. Needs ~7 GB free for the output copy.
set -euo pipefail

IN="${1:?usage: remaster-noprompt.sh <input.iso> <output.iso>}"
OUT="${2:?usage: remaster-noprompt.sh <input.iso> <output.iso>}"

SECTOR=2048

echo "==> Locating the UEFI El Torito boot image in $IN…"
# -report_el_torito plain lists each boot image with its platform + start LBA +
# block count. We want the UEFI entry's LBA and size.
ELT="$(xorriso -indev "$IN" -report_el_torito plain 2>/dev/null \
  | grep "El Torito boot img" || true)"
echo "$ELT"
UEFI_LINE="$(echo "$ELT" | awk '/UEFI/{print; exit}')"
[ -n "$UEFI_LINE" ] || { echo "ERROR: no UEFI El Torito boot image found"; exit 1; }
# Columns: "El Torito boot img :  N  Pltf  B  Emul  Ld_seg Hdpt Ldsiz  LBA"
UEFI_LBA="$(echo "$UEFI_LINE" | awk '{print $NF}')"
echo "==> UEFI boot image at LBA $UEFI_LBA."

# The El Torito "Ldsiz" (load size, in 512-byte virtual sectors) for Windows
# media is small/bogus; the real embedded image is a full 1.44 MB floppy image.
# We replace the whole 1474560-byte region (720 × 2048-byte ISO sectors).
NOPROMPT_BYTES=1474560
NOPROMPT_SECTORS=$(( NOPROMPT_BYTES / SECTOR ))   # 720

echo "==> Reading efisys_noprompt.bin from the media (loop mount)…"
MNT="$(mktemp -d)"
sudo mount -o loop,ro "$IN" "$MNT"
trap 'sudo umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true' EXIT
NOPROMPT_SRC="$(find "$MNT" -ipath '*efi/microsoft/boot/efisys_noprompt.bin' -print -quit)"
[ -n "$NOPROMPT_SRC" ] || { echo "ERROR: efisys_noprompt.bin not on media"; exit 1; }
NP_SIZE="$(stat -c %s "$NOPROMPT_SRC")"
[ "$NP_SIZE" -eq "$NOPROMPT_BYTES" ] || {
  echo "ERROR: efisys_noprompt.bin is $NP_SIZE bytes, expected $NOPROMPT_BYTES"; exit 1; }
TMP_NP="$(mktemp)"
cp "$NOPROMPT_SRC" "$TMP_NP"
sudo umount "$MNT" 2>/dev/null || true; rmdir "$MNT" 2>/dev/null || true
trap 'rm -f "$TMP_NP"' EXIT

# Safety check: confirm the bytes currently at UEFI_LBA match efisys.bin (i.e.
# we're about to overwrite the boot image, not random data). Both efisys*.bin
# start with the same FAT12 BPB, so compare against efisys.bin from the media.
echo "==> Verifying the target region is the efisys boot image…"
MNT2="$(mktemp -d)"; sudo mount -o loop,ro "$IN" "$MNT2"
EFISYS_SRC="$(find "$MNT2" -ipath '*efi/microsoft/boot/efisys.bin' -print -quit)"
if [ -n "$EFISYS_SRC" ]; then
  if dd if="$IN" bs="$SECTOR" skip="$UEFI_LBA" count="$NOPROMPT_SECTORS" 2>/dev/null \
       | cmp -s - "$EFISYS_SRC"; then
    echo "    OK: region at LBA $UEFI_LBA == efisys.bin (prompt image)."
  else
    echo "ERROR: bytes at LBA $UEFI_LBA do not match efisys.bin — aborting to"
    echo "       avoid corrupting the ISO. (El Torito layout unexpected.)"
    sudo umount "$MNT2" 2>/dev/null || true; rmdir "$MNT2" 2>/dev/null || true
    exit 1
  fi
fi
sudo umount "$MNT2" 2>/dev/null || true; rmdir "$MNT2" 2>/dev/null || true

echo "==> Copying $IN → $OUT…"
cp "$IN" "$OUT"

echo "==> Patching the UEFI boot image in place (no-prompt)…"
dd if="$TMP_NP" of="$OUT" bs="$SECTOR" seek="$UEFI_LBA" count="$NOPROMPT_SECTORS" \
   conv=notrunc 2>&1 | tail -1

echo "==> Verifying the patch…"
if dd if="$OUT" bs="$SECTOR" skip="$UEFI_LBA" count="$NOPROMPT_SECTORS" 2>/dev/null \
     | cmp -s - "$TMP_NP"; then
  echo "    OK: UEFI boot image is now efisys_noprompt.bin."
else
  echo "ERROR: post-patch verification failed."; exit 1
fi

echo "==> Done: $OUT"
echo "    The DVD now boots Windows Setup directly — no 'press any key' prompt."
