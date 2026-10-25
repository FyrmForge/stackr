package service

import "context"

// PanelBackupOffBox reports whether the scheduled panel backup goes to a
// destination other than this server's disk. A dest that is unset, gone or
// the local one all answer false: those archives die with the box.
func (o *Orchestrator) PanelBackupOffBox(ctx context.Context) (bool, error) {
	id, err := o.settings.Get(ctx, "panel_backup_dest")
	if err != nil || id == "" {
		return false, err
	}
	ds, err := o.backups.Global(ctx)
	if err != nil {
		return false, err
	}
	for _, d := range ds {
		if d.ID == id {
			return d.Kind != "local", nil
		}
	}
	return false, nil
}
