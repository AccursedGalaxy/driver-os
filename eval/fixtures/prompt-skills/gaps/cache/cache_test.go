package cache

import "testing"

func TestPutGet(t *testing.T) {
	c := New(2)
	c.Put("a", 1)
	c.Put("b", 2)
	v, ok := c.Get("a")
	if !ok || v != 1 {
		t.Fatal("a should be present with value 1")
	}
}

func TestOverwrite(t *testing.T) {
	c := New(2)
	c.Put("a", 1)
	c.Put("a", 2)
	v, ok := c.Get("a")
	if !ok || v != 2 {
		t.Fatal("overwrite should update the stored value")
	}
}
