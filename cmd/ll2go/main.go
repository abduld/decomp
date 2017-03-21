package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"go/ast"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path/filepath"

	"github.com/decomp/decomp/cfa/primitive"
	"github.com/llir/llvm/asm"
	"github.com/llir/llvm/ir"
	"github.com/llir/llvm/ir/constant"
	"github.com/llir/llvm/ir/value"
	"github.com/mewkiz/pkg/pathutil"
	"github.com/pkg/errors"
)

func main() {
	flag.Parse()
	for _, llPath := range flag.Args() {
		if err := ll2go(llPath); err != nil {
			log.Fatalf("%+v", err)
		}
	}
}

// ll2go converts the given LLVM IR assembly file into a corresponding Go source
// file.
func ll2go(llPath string) error {
	module, err := asm.ParseFile(llPath)
	if err != nil {
		return errors.WithStack(err)
	}
	srcName := pathutil.FileName(llPath)
	d := newDecompiler()
	file := &ast.File{
		Name: ast.NewIdent(srcName),
	}
	for _, f := range module.Funcs {
		prims, err := parsePrims(srcName, f.Name)
		if err != nil {
			return errors.WithStack(err)
		}
		fn, err := d.funcDecl(f, prims)
		if err != nil {
			return errors.WithStack(err)
		}
		file.Decls = append(file.Decls, fn)
		// TODO: Remove debug output.
		if err := printer.Fprint(os.Stdout, token.NewFileSet(), fn); err != nil {
			return errors.WithStack(err)
		}
		fmt.Println()
	}
	return nil
}

// A decompiler keeps track of relevant information during the decompilation
// process.
type decompiler struct {
	// Map from basic block label to conceptual basic block.
	blocks map[string]*basicBlock
}

// newDecompiler returns a new decompiler.
func newDecompiler() *decompiler {
	return &decompiler{}
}

// funcDecl converts the given LLVM IR function into a corresponding Go function
// declaration.
func (d *decompiler) funcDecl(f *ir.Function, prims []*primitive.Primitive) (*ast.FuncDecl, error) {
	// Force generate local IDs.
	_ = f.String()

	// Recover function declaration.
	typ := d.goType(f.Sig)
	sig := typ.(*ast.FuncType)
	fn := &ast.FuncDecl{
		Name: d.global(f.Name),
		Type: sig,
	}
	if len(f.Blocks) < 1 {
		return fn, nil
	}

	// Reset basic block mapping.
	d.blocks = make(map[string]*basicBlock)
	for _, block := range f.Blocks {
		d.blocks[block.Name] = &basicBlock{BasicBlock: block}
	}

	// Record outgoing PHI values.
	for _, block := range f.Blocks {
		for _, inst := range block.Insts {
			phi, ok := inst.(*ir.InstPhi)
			if !ok {
				continue
			}
			// The incoming values of PHI instructions are propagated as assignment
			// statements to the predecessor basic blocks of the incoming values.
			for _, inc := range phi.Incs {
				pred := d.blocks[inc.Pred.Name]
				assignStmt := &ast.AssignStmt{
					Lhs: []ast.Expr{d.local(phi.Name)},
					Tok: token.ASSIGN,
					Rhs: []ast.Expr{d.value(inc.X)},
				}
				pred.out = append(pred.out, assignStmt)
			}
		}
	}

	// Recover control flow primitives.
	for _, prim := range prims {
		block, err := d.prim(prim)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		// Delete merged basic blocks.
		for _, node := range prim.Nodes {
			delete(d.blocks, node)
		}
		// Add primitive basic block.
		d.blocks[block.Name] = block
	}

	// After control flow recovery, a single basic block should remain.
	var block *basicBlock
	if len(d.blocks) != 1 {
		return nil, errors.Errorf("control flow recovery failed; unable to reduce function into a single basic block; expected 1 basic block, got %d", len(d.blocks))
	}
	for _, b := range d.blocks {
		block = b
	}

	// Recover function body.
	block.stmts = append(block.stmts, d.term(block.Term))
	body := &ast.BlockStmt{
		List: block.stmts,
	}
	fn.Body = body
	return fn, nil
}

// global converts the given LLVM IR global identifier to a corresponding Go
// identifier.
func (d *decompiler) global(name string) *ast.Ident {
	return ast.NewIdent(name)
}

// local converts the given LLVM IR local identifier to a corresponding Go
// identifier.
func (d *decompiler) local(name string) *ast.Ident {
	name = "_" + name
	return ast.NewIdent(name)
}

// value converts the given LLVM IR value to a corresponding Go expression.
func (d *decompiler) value(v value.Value) ast.Expr {
	switch v := v.(type) {
	case value.Named:
		switch v.(type) {
		case *ir.Global, *ir.Function:
			return d.global(v.GetName())
		default:
			return d.local(v.GetName())
		}
	case constant.Constant:
		switch v := v.(type) {
		case *constant.Int:
			return &ast.BasicLit{
				Kind:  token.INT,
				Value: v.X.String(),
			}
		default:
			panic(fmt.Sprintf("support for constant value %T not yet implemented", v))
		}
	default:
		panic(fmt.Sprintf("support for value %T not yet implemented", v))
	}
}

// basicBlock represents a conceptual basic block, that may contain both LLVM IR
// instructions and Go statements.
type basicBlock struct {
	*ir.BasicBlock
	// Go statements.
	stmts []ast.Stmt
	// Outgoing values for PHI instructions. In other words, a list of assignment
	// statements to appear at the end of the basic block.
	out []ast.Stmt
}

// stmts converts the basic block instructions, recorded statements and outgoing
// PHI values into a corresponding list of Go statements.
func (d *decompiler) stmts(block *basicBlock) []ast.Stmt {
	var stmts []ast.Stmt
	stmts = append(stmts, d.insts(block.Insts)...)
	stmts = append(stmts, block.stmts...)
	stmts = append(stmts, block.out...)
	return stmts
}

// parsePrims parses the JSON file containing a mapping of control flow
// primitives for the given function.
func parsePrims(srcName, funcName string) ([]*primitive.Primitive, error) {
	// TODO: Generate prims if not present on file system.
	graphsDir := fmt.Sprintf("%s_graphs", srcName)
	jsonName := funcName + ".json"
	jsonPath := filepath.Join(graphsDir, jsonName)
	var prims []*primitive.Primitive
	f, err := os.Open(jsonPath)
	if err != nil {
		return nil, errors.WithStack(err)
	}
	defer f.Close()
	r := bufio.NewReader(f)
	dec := json.NewDecoder(r)
	if err := dec.Decode(&prims); err != nil {
		return nil, errors.WithStack(err)
	}
	return prims, nil
}