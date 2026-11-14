# Postgres extensions

An operated postgres instance lists the extensions its slices may use. Each slice lists the ones its database gets. stackr creates them as the superuser at deploy, so an app that is not a superuser (Immich, for one) still gets them. Taking a name off a list never drops the extension.

The instance holds two lists:

- Extensions: what slices may ask for.
- Startup modules: libraries postgres loads at start (`shared_preload_libraries`). `pg_stat_statements` is always loaded, so leave it out.

## Get the files onto the instance

Two ways.

- Recipes. stackr builds an image from the official postgres image plus the packages a recipe names. Built in: `pgvector`, `postgis` and `vchord`. An admin adds more (below). Recipes need the official image.
- A custom image. You bring an image built on the official Debian postgres image whose tag starts with the major. You list its startup modules yourself. A name the image does not hold is refused and nothing changes.

## Panel

### Instance

Instance drawer, Settings tab, Extensions row.

1. Image shows the official image, the image stackr built (and from which recipes), or a field for a custom image.
2. List the extensions, one per line. For a recipe-built instance the startup modules come from the recipes and the field is locked. For a custom image, list them in Startup modules.
3. Check. It shows a plan: the build, package versions that change, a Postgres minor update the extension needs, the slices on the instance, and anything that blocks it.
4. Apply plan. A plan that restarts the instance has a "Restart the instance" box to tick. Apply builds the image, boots a scratch cluster to check it, restarts the instance, then creates the missing extensions and updates the installed ones.

If the real server does not start, the previous image and startup list are put back.

On an instance that has not deployed yet, Apply saves the lists and the next deploy applies them.

The Overview tab lists the extensions with their versions, which are startup modules, and the slices that use each.

An extension cannot leave the instance's list while a slice lists it. Take it off the slice first.

### Slice

Slice drawer, Extensions row. Tick from what the instance allows, Save. Dependencies are added with it (CASCADE) and shown as "added with". A name the instance does not list, or its image does not hold, is refused. The same holds for a branch to another instance. If the stack is run from a stack file the row is read-only.

Unticking leaves the extension in the database.

### Admin recipes

Admin, Postgres extensions. Built in lists `pgvector`, `postgis` and `vchord`, each with Switch off or Switch on. Switching off stops new enables only. Instances that have the recipe keep it and can rebuild.

Your recipes lists the ones you added, with Edit and delete. Add a recipe takes:

| Field | Holds |
| --- | --- |
| Name | The recipe's name. |
| Postgres versions | Majors it supports, comma separated. |
| Packages | Apt package names, one per line. `{major}` stands for the postgres major. |
| Package URL, Version, sha256 | A https download of a `.deb` instead of packages. Needs a sha256 for each major and CPU type, like `17-amd64=hex`. |
| Extensions it creates | Names `CREATE EXTENSION` takes. |
| Startup modules | Libraries it needs loaded. |
| Needs recipes | Recipes built in with it. |

Set packages or a package URL, not both. Sharing sets who may use it: everyone, or chosen orgs. A recipe still enabled on an instance cannot be deleted.

## Example: Immich on the official image

1. Admin has `pgvector` and `vchord` on. `vchord` needs pgvector and runs on postgres 17 or 18.
2. Create a postgres instance on 17 or 18 and deploy it.
3. Instance drawer, Settings, Extensions: list `vchord`, `vector`, `cube` and `earthdistance`. Check, then Apply plan with the restart ticked. `vchord` becomes a startup module by itself.
4. Add a slice tile on it and, in the slice drawer, tick all four. Deploy the slice.
5. Point Immich at the slice with a ref such as `${{ tile.immich-db.url }}`, as in [What apps read](operated.md#what-apps-read).

Set `vchordrq.probes` per session in the app. Setting it server-wide breaks some index shapes.

## CLI

The same actions. `stackr operated` has the alias `opr`.

```bash
stackr operated extensions get pg
stackr operated extensions plan pg vchord vector cube earthdistance
stackr operated extensions apply pg --plan <id from plan>
stackr slice extensions get immich-db
stackr slice extensions set immich-db vchord vector cube earthdistance
```

`plan` takes the whole list the instance should have and prints the plan and its id. `apply` needs that id and asks before a plan that restarts the instance; `--yes` confirms it. `get` shows the image, the startup modules and the extensions.

`stackr operated extensions set pg ...` sets the lists directly, for the official image or a custom one. `--image` sets a custom image, `--preload` its startup modules (`--preload ""` empties them). With no names and no flags the list is emptied. A recipe-built instance takes its extensions through `plan` and `apply`.

Admin, for recipes:

```bash
stackr server pg-extensions list
stackr server pg-extensions set pgvector --off
stackr server pg-extensions set mine --major 17 --major 18 \
  --package 'postgresql-{major}-mine' --extension mine --all
stackr server pg-extensions rm mine
```

`set` also takes `--package-url`, `--sha256 17-amd64=hex` (repeatable), `--version`, `--preload`, `--requires`, `--org` (repeatable) and `--on` or `--off`. `rm` on a built-in removes its switch-off row.

`--no-wait` prints the job and returns. Without it the CLI follows the job.

## Files

### Server file

Recipes go in `stackr-server.yml` under `pg_extensions`, by name:

```yaml
pg_extensions:
  mine:
    majors: [16, 17]
    packages: ['postgresql-{major}-mine']
    extensions: [mine]
    preload: [mine]
    share: all
  vchord:
    enabled: false
  pgvector:
    enabled: false
```

A package pattern with `{major}` inside a YAML `[...]` list must be quoted, as above. Unquoted, YAML reads the braces as a map.

An own recipe may use `package_url` with `version` and `sha256` instead of `packages`. `sha256` keys are `<major>-<arch>`, like `17-amd64`. `share` is `all` or a list of org slugs. A built-in takes only `enabled: false`. A recipe the block no longer names shows as a removal row in the plan.

### Stack file

On an operated postgres tile:

```yaml
base:
  tiles:
    pg:
      engine: postgres
      major: 17
      extensions: [vchord, vector, cube, earthdistance]
    immich-db:
      type: slice
      provision_from: shop:dev:pg
      extensions: [vchord, vector, cube, earthdistance]
```

- `extensions` on the instance: what slices may list.
- `preload` on the instance: its startup modules, after `pg_stat_statements`. A recipe-built instance takes them from its recipes, and a file that changes them is blocked.
- `extensions` on a slice: what its database gets.

A key left out leaves the list as it is. An empty list clears it. The instance's image is not set by hand once it is recipe-built. A custom image may change within its major. The plan shows an extension change as a row and, for a recipe build, runs the same build plan the panel's Check does. Both keys are in [Config files](config-files.md#operated-tile).

## Upgrades

- Minor update: an instance with recipes updates through a plan. It rebuilds on the new minor. If an extension's package needs a newer postgres minor than the instance runs, the plan offers it together with that update.
- Major upgrade: works for a recipe-built instance. stackr builds the new major's image from the same recipes, and a pinned helper that holds the extension files of both majors. If a recipe has no build for the target major, the upgrade is refused and names the recipe. Missing extensions are created and updated afterwards. See [Versions](operated.md#versions).
- A custom-image instance refuses a major change. Make a new instance on the new major and branch the slices to it.

Recipe builds are kept with the instance's backups, so a restore does not rebuild.

## Limits

- Recipes need Debian-based images. A custom image must be Debian-based too.
- Names must be lower case letters, digits and `_`. A name with a hyphen, like `uuid-ossp`, is not accepted.
- A startup module runs in every database on the instance. A change to the startup modules restarts the instance, and a crash in a module restarts every slice.
- An instance with a standby refuses a change to the image or the startup modules. Remove the standby, change it, add the standby again.
- A recipe build fetches its packages: the build needs egress to the package sources (the Debian and PGDG mirrors, or the package URL's host).
- Extension settings are instance-wide. Set them in the instance's Postgres settings. There are no per-slice settings.
- Whoever can list an extension that needs the superuser on a slice gets a possible route to superuser on that instance. Cut slices only for stacks you trust: the [allow list](operated.md#allow-list) controls who may.
