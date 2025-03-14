package controllers

const (
	KubevirtMachineTemplateReconciledCondition = "KubevirtMachineTemplateReconciled"

	WaitingForDataVolumeReason = "WaitingForDataVolume"
	WaitingForDataSourceReason = "WaitingForDataSource"
)

const (
	TalosControlPlaneReconciledCondition = "TalosControlPlaneReconciled"

	WaitingForTLSRouteReason = "WaitingForTLSRoute"
)

const (
	TalosConfigTemplateReconciledCondition = "TalosConfigTemplateReconciled"
)

const (
	MachineDeploymentReconciledCondition = "MachineDeploymentReconciled"
)

const (
	ReconciledReason = "Reconciled"
)
