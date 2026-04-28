# test-replays/

Drop `.dem` files here to run the regression test (`go test`). Pair each replay
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
