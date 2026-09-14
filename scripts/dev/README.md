# Development deployment helpers

These scripts operate disposable remote test machines. They are not part of
the product installer and have no production safety guarantees.

Set `STACKR_DEPLOY_HOST` explicitly before using them:

```bash
export STACKR_DEPLOY_HOST=manager.example.test
scripts/dev/deploy-test.sh
scripts/dev/wipe-test.sh --nodes worker.example.test
```

- `deploy-test.sh` builds locally and replaces the remote test service.
- `wipe-test.sh` destroys the test Swarm and its data.
- `connector-keep.sh` preserves a test GitHub App installation across wipes.
- `vm.sh` manages one disposable local libvirt VM.
