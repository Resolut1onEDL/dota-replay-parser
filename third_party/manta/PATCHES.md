# Patched manta

`github.com/dotabuff/manta` v1.4.7 (module cache copy; upstream tests,
`fixtures/`, `replays/` and `.github/` left out) plus the patch below. The
parser's `go.mod` points at this directory with a `replace` line.

## fixed8 decoder (2026-10-07)

Symptom: match 9032897977 died at tick 2200 with
`Parse error: nextByte: insufficient buffer (380 of 379)` while reading the
CParticleSystem instance baseline (entity.go, `readFields(newReader(baseline), …)`).

Cause: since Dota build 6951 (2026-10-07; build 6944 had none) serializers tag
~25k fields with the var encoder `fixed8` (uint8, int8, enums like MoveType_t,
`CNetworkUtlVectorBase< uint8 >`); the server writes them as 8 raw bits. manta picks decoders by type name only and read
them as varints. A one-byte varint equals the raw byte while the value is
below 128; 255 does not, and every build-6951 replay creates CParticleSystem
entities (a public turbo game, 9033035139, failed the same way). CParticleSystem's
`m_iServerControlPointAssignments` (uint8[4]) defaults to 255, its first
element swallowed five bytes, and the baseline ran out of buffer at
`m_hControlPointEnts.0062`. int8 fields also came out wrong (zigzag: 41 → -21).

Fix: `field_decoder.go` — `fixed8Decoder`, chosen first in `findDecoder`;
`field.go` — vector elements of a fixed8 `CNetworkUtlVectorBase` use it too
(the length stays a varint). Value Go types are unchanged (uint64 for uint8,
int32 for int8, uint32 for the rest). Same rule as clarity
(skadistats/clarity 17b33df, "honor fixed8 encoder on any S2 field type").
Upstream manta has no fix as of 2026-10-07 (v1.5.0 reads these as varints);
the same patch for master is dotabuff/manta#182.

Tests: `cd third_party/manta && go test -run Fixed8 .`; replay regression
`TestFixed8ParticleBaselineReplay` in the parser's `main_test.go`.

## Dropping the patch

Once upstream manta honors fixed8: bump manta in `go.mod`, delete the
`replace` line and this directory, the `COPY third_party` line in `Dockerfile`
and the `third_party` rsync in `scripts/release-sync.sh`, then run
`TestFixed8ParticleBaselineReplay` with the 9032897977 replay in place.
