# Architecture Diagrams

[C4 model](https://c4model.com) diagrams for the DNS operator, authored in
[C4-PlantUML](https://github.com/plantuml-stdlib/C4-PlantUML) and styled with the
shared Datum brand theme (`datum-theme.puml`, copied from
[datum-cloud/enhancements](https://github.com/datum-cloud/enhancements)).

| Source | Rendered | Used by |
|--------|----------|---------|
| `system-context.puml` | `system-context.png` | [Architecture Overview](../README.md) |
| `container-view.puml` | `container-view.png` | [Deployment Topology](../topology.md) |
| `replication-flow.puml` | `replication-flow.png` | [Replication Model](../replication.md) |
| `powerdns-backend.puml` | `powerdns-backend.png` | [PowerDNS Backend](../backends/powerdns.md) |

## Editing

Edit the `.puml` source, then re-render the committed PNGs with Docker (no local
Java or PlantUML install required):

```sh
# from the repo root
task -t docs/Taskfile.yaml diagrams          # render all
task -t docs/Taskfile.yaml diagrams:validate # syntax-check only
```

The PNGs are committed alongside their source so they render on GitHub without a
build step. Regenerate and commit them whenever a `.puml` changes.
