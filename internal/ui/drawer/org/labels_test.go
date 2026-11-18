package org

import "testing"

// The org drawer has nine tabs; at desktop width they all show only while the labels stay short.
func TestTabLabelsStayShort(t *testing.T) {
	for k, l := range Labels {
		if len(l) > 12 {
			t.Errorf("tab %s label %q is too long for the tab row", k, l)
		}
	}
}
