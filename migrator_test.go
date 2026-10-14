package dm

import "testing"

func TestGroupByIndexName(t *testing.T) {
	rows := []*Index{
		{TableName: "T1", IndexName: "IDX_A", ColumnName: "C1"},
		{TableName: "T1", IndexName: "IDX_A", ColumnName: "C2"},
		{TableName: "T1", IndexName: "IDX_B", ColumnName: "C3"},
	}

	grouped := groupByIndexName(rows)
	if len(grouped) != 2 {
		t.Fatalf("group count = %d, want 2", len(grouped))
	}
	if len(grouped["IDX_A"]) != 2 {
		t.Fatalf("IDX_A columns = %d, want 2", len(grouped["IDX_A"]))
	}
	if grouped["IDX_A"][0].ColumnName != "C1" || grouped["IDX_A"][1].ColumnName != "C2" {
		t.Fatalf("IDX_A column order = [%s %s], want [C1 C2]",
			grouped["IDX_A"][0].ColumnName, grouped["IDX_A"][1].ColumnName)
	}
}
