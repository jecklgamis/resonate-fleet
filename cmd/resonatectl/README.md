# resonatectl

The command-line client for a resonate-controller: run a scenario on every worker
registered with it, and list the workers.

```sh
export RESONATE_CONTROLLER=https://controller.example.com CONTROLLER_TOKEN=...
resonatectl workers
resonatectl run scenario.yaml
```

Full documentation: [docs/resonatectl.md](../../docs/resonatectl.md).
