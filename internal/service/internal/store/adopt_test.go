package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
)

// Envs made before nodes existed (node_id ”) are placed on the given node;
// placed ones stay where they are.
func TestAdoptUnplaced(t *testing.T) {
	st := storetest.Store(t)
	now := time.Now()
	for _, q := range []string{
		`INSERT INTO orgs VALUES ('o1','Acme','acme','','','{}',NULL,'',?,'','','','',0,NULL)`,
		`INSERT INTO stacks VALUES ('s1','o1','Shop','shop','','{}','','','','',?,NULL)`,
		`INSERT INTO environments VALUES ('e1','s1','Dev','dev','static',NULL,'{}','',0,'net',NULL,'branch','main',1,?,1,'','','')`,
		`INSERT INTO environments VALUES ('e2','s1','Prod','prod','static',NULL,'{}','',1,'net2',NULL,'branch','main',1,?,1,'w1','','')`,
	} {
		if _, err := st.DB().Exec(q, now); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	n, err := st.Environments.AdoptUnplaced(ctx, "ctl")
	if err != nil || n != 1 {
		t.Fatalf("adopted %d, %v; want 1", n, err)
	}
	e1, _ := st.Environments.Get(ctx, "e1")
	e2, _ := st.Environments.Get(ctx, "e2")
	if e1.NodeID != "ctl" || e2.NodeID != "w1" {
		t.Errorf("e1 %q e2 %q", e1.NodeID, e2.NodeID)
	}
}
