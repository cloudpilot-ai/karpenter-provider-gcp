# Future e2e test proposal

## P1

### Network configurations
- Private cluster coverage with Workload Identity enabled
- Custom subnet and secondary range selection via `subnetRangeName`

### Disk size validation
- Explicit boot disk sizing checks for `pd-ssd` and `pd-balanced`
- Regression coverage for the SSD sizing issue so invalid disk settings fail fast

## P2

### Multi-network scenarios
- Multiple subnet ranges per cluster
- Distinct `networkTags` validation across NodeClasses

### Consolidation behavior
- Scale-down after workload deletion
- Node replacement when a cheaper or better-fit node becomes available

### Drift detection
- NodeClass mutation triggers controlled node replacement
- NodePool requirement changes force drifted nodes to roll

## P3

### Interruption handling
- Spot preemption simulation with validation of cordon, drain, and replacement behavior

### Custom machine types and accelerators
- Custom machine type provisioning
- GPU node provisioning and scheduling coverage

## P4

### Performance and load testing
- Burst provisioning for 100+ nodes
- Controller latency and scheduling throughput under sustained load

### Regional vs zonal provisioning
- Compare node selection and provisioning behavior between regional and zonal clusters
