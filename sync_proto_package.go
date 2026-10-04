package main

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/bufbuild/protocompile/ast"
	"github.com/bufbuild/protocompile/parser"
	"github.com/bufbuild/protocompile/reporter"
)

// declaredServiceGoPackage reads the option from the file that declares the
// scaffolded service, rather than from an imported descriptor. Existing APIs
// can live in versioned Go packages that differ from the factory's default.
func declaredServiceGoPackage(root, expectedService string) (string, error) {
	var result string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".proto" {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		node, parseErr := parser.Parse(path, file, reporter.NewHandler(nil))
		closeErr := file.Close()
		if parseErr != nil {
			return parseErr
		}
		if closeErr != nil {
			return closeErr
		}
		ownsService := false
		goPackage := ""
		for _, declaration := range node.Decls {
			switch declaration := declaration.(type) {
			case *ast.ServiceNode:
				ownsService = ownsService || declaration.Name.Val == expectedService
			case *ast.OptionNode:
				if len(declaration.Name.Parts) != 1 || declaration.Name.Parts[0].IsExtension() || declaration.Name.Parts[0].Value() != "go_package" {
					continue
				}
				value, ok := declaration.Val.(ast.StringValueNode)
				if !ok {
					return fmt.Errorf("go_package in %s must be a string", path)
				}
				goPackage = value.AsString()
			}
		}
		if ownsService {
			result = goPackage
		}
		return nil
	})
	return result, err
}
