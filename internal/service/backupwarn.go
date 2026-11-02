package service

import "context"

// PanelBackupOffBox reports whether the scheduled panel backup leaves this
// server's disk: an off-box admin destination exists, either named in
// panel_backup_dest or receiving a copy of the local backup (see
// panelBackup). Otherwise those archives die with the box.
func (o *Orchestrator) PanelBackupOffBox(ctx context.Context) (bool, error) {
	_, ok, err := o.offBoxDest(ctx)
	return ok, err
}
