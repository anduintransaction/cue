package openapi

import (
	"cuelang.org/go/cue"
	"cuelang.org/go/cue/ast"
)

func (b *builder) updateNullableType() {
	if b.core != nil {
		return
	}
	b.nullable = true
}

func (b *builder) setNullable() bool {
	if b.ctx.version == "3.0.0" {
		return true
	} else {
		b.updateNullableType()
		return false
	}
}

func (b *builder) setFixedLengthItems(items []ast.Expr) {
	if b.ctx.version == "3.0.0" {
		b.set("items", ast.NewList(items...))
	} else {
		b.set("prefixItems", ast.NewList(items...))
	}
}

func (b *builder) setRemainingItems(items *ast.StructLit, hasPrefix bool) {
	if b.ctx.version == "3.0.0" {
		if hasPrefix {
			b.setFilter("Schema", "additionalItems", items) // Not allowed in structural.
		} else if !b.isNonCore() || len(items.Elts) > 0 {
			b.setSingle("items", items, true)
		}
	} else {
		b.set("items", items)
	}
}

func (b *builder) setLessThan(v cue.Value) {

}
