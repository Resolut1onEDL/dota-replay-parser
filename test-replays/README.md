# test-replays/

Drop `.dem` (or `.dem.bz2`, as Valve serves them) files here to run the regression tests (`go test`:
team assignment and the audit's exact fields, `TestAuditAgainstOpenDota`). Pair each replay
with its OpenDota ground-truth JSON named `<match_id>_opendota.json`:

```
test-replays/
  8582691771.dem
  8582691771_opendota.json
  ...
```

The OpenDota JSON is fetched via:

```sh
curl -s "https://api.opendota.com/api/matches/<match_id>" > <match_id>_opendota.json
```

(For matches not yet parsed by OpenDota, hit `POST /api/request/<match_id>`
first, wait a minute, then GET.)

`.dem` and `.dem.bz2` files are gitignored — they're large (50–100 MB) and
shouldn't be committed.

`TestFixed8ParticleBaselineReplay` needs `9032897977.dem.bz2` (no OpenDota JSON,
the expected numbers are in the test):

```sh
curl -o test-replays/9032897977.dem.bz2 http://replay151.valve.net/570/9032897977_1496998234.dem.bz2
```
