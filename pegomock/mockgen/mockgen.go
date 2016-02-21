// Copyright 2015 Peter Goetz
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Based on the work done in
// https://github.com/golang/mock/blob/d581abfc04272f381d7a05e4b80163ea4e2b9447/mockgen/mockgen.go

// MockGen generates mock implementations of Go interfaces.
package mockgen

// TODO: This does not support recursive embedded interfaces.
// TODO: This does not support embedding package-local interfaces in a separate file.

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"io"
	"io/ioutil"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/petergtz/pegomock/pegomock/mockgen/model"
)

const importPath = "github.com/petergtz/pegomock"

func GenerateMock(packagePath, interfaceName, outputDirPath, packageOut string) (bool, string) {
	output := generateMockSourceCode([]string{packagePath, interfaceName}, packageOut, "", false, os.Stdout)
	outputFilepath := outputFilePath([]string{packagePath, interfaceName}, outputDirPath, "") // <- adjust last param

	existingFileContent, err := ioutil.ReadFile(outputFilepath)
	if err != nil {
		if os.IsNotExist(err) {
			err = ioutil.WriteFile(outputFilepath, output, 0664)
			panicOnError(err)
			return true, outputFilepath
		} else {
			panic(err)
		}
	}
	if string(existingFileContent) == string(output) {
		return false, outputFilepath
	} else {
		err = ioutil.WriteFile(outputFilepath, output, 0664)
		panicOnError(err)
		return true, outputFilepath
	}
}

func GenerateMockFileInOutputDir(
	args []string,
	outputDirPath string,
	outputFilePathOverride string,
	packageOut string,
	selfPackage string,
	debugParser bool,
	out io.Writer) {
	GenerateMockFile(
		args,
		outputFilePath(args, outputDirPath, outputFilePathOverride),
		packageOut,
		selfPackage,
		debugParser,
		out)
}

func outputFilePath(args []string, outputDirPath string, outputFilePathOverride string) string {
	if outputFilePathOverride != "" {
		return outputFilePathOverride
	} else if sourceMode(args) {
		return filepath.Join(outputDirPath, "mock_"+strings.TrimSuffix(args[0], ".go")+"_test.go")
	} else {
		return filepath.Join(outputDirPath, "mock_"+strings.ToLower(args[len(args)-1])+"_test.go")
	}
}

func GenerateMockFile(args []string, outputFilePath string, packageOut string, selfPackage string, debugParser bool, out io.Writer) {
	output := generateMockSourceCode(args, packageOut, selfPackage, debugParser, out)

	err := ioutil.WriteFile(outputFilePath, output, 0664)
	if err != nil {
		panic(fmt.Errorf("Failed writing to destination: %v", err))
	}
}

func generateMockSourceCode(args []string, packageOut string, selfPackage string, debugParser bool, out io.Writer) []byte {
	var err error

	var ast *model.Package
	var src string
	if sourceMode(args) {
		ast, err = ParseFile(args[0])
		src = args[0]
	} else {
		if len(args) != 2 {
			log.Fatal("Expected exactly two arguments, but got " + fmt.Sprint(args))
		}
		ast, err = Reflect(args[0], strings.Split(args[1], ","))
		src = fmt.Sprintf("%v (interfaces: %v)", args[0], args[1])
	}
	if err != nil {
		panic(fmt.Errorf("Loading input failed: %v", err))
	}

	if debugParser {
		ast.Print(out)
	}

	output, err := generateOutput(ast, src, packageOut, selfPackage)
	if err != nil {
		panic(fmt.Errorf("Failed generating mock: %v", err))
	}
	return output
}

func sourceMode(args []string) bool {
	if len(args) == 1 && strings.HasSuffix(args[0], ".go") {
		return true
	}
	return false
}

type generator struct {
	buf    bytes.Buffer
	indent string

	packageMap map[string]string // map from import path to package name
}

func (g *generator) p(format string, args ...interface{}) *generator {
	fmt.Fprintf(&g.buf, g.indent+format+"\n", args...)
	return g
}

func (g *generator) in() *generator {
	g.indent += "\t"
	return g
}

func (g *generator) out() *generator {
	if len(g.indent) > 0 {
		g.indent = g.indent[0 : len(g.indent)-1]
	}
	return g
}

func removeDot(s string) string {
	if len(s) > 0 && s[len(s)-1] == '.' {
		return s[0 : len(s)-1]
	}
	return s
}

// sanitize cleans up a string to make a suitable package name.
// pkgName in reflect mode is the base name of the import path,
// which might have characters that are illegal to have in package names.
func sanitize(s string) string {
	t := ""
	for _, r := range s {
		if t == "" {
			if unicode.IsLetter(r) || r == '_' {
				t += string(r)
				continue
			}
		} else {
			if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
				t += string(r)
				continue
			}
		}
		t += "_"
	}
	if t == "_" {
		t = "x"
	}
	return t
}

func generateOutput(ast *model.Package, source string, packageOut string, selfPackage string) ([]byte, error) {
	g := new(generator)
	if err := g.Generate(source, ast, packageOut, selfPackage); err != nil {
		return nil, fmt.Errorf("Failed generating mock: %v", err)
	}
	return g.Output(), nil
}

func (g *generator) Generate(source string, pkg *model.Package, pkgName string, selfPackage string) error {
	g.p("// Automatically generated by MockGen. DO NOT EDIT!")
	g.p("// Source: %v", source)
	g.p("")

	// Get all required imports, and generate unique names for them all.
	im := pkg.Imports()
	im[importPath] = true
	g.packageMap = make(map[string]string, len(im))
	localNames := make(map[string]bool, len(im))
	for pth := range im {
		base := sanitize(path.Base(pth))

		// Local names for an imported package can usually be the basename of the import path.
		// A couple of situations don't permit that, such as duplicate local names
		// (e.g. importing "html/template" and "text/template"), or where the basename is
		// a keyword (e.g. "foo/case").
		// try base0, base1, ...
		pkgName := base
		i := 0
		for localNames[pkgName] || token.Lookup(pkgName).IsKeyword() {
			pkgName = base + strconv.Itoa(i)
			i++
		}

		g.packageMap[pth] = pkgName
		localNames[pkgName] = true
	}

	g.p("package %v", pkgName)
	g.p("")
	g.p("import (")
	g.in()
	for path, pkg := range g.packageMap {
		if path == selfPackage {
			continue
		}
		g.p("%v %q", pkg, path)
	}
	for _, path := range pkg.DotImports {
		g.p(". %q", path)
	}
	g.out()
	g.p(")")

	for _, iface := range pkg.Interfaces {
		g.GenerateMockInterface(iface, selfPackage)
	}

	return nil
}

// The name of the mock type to use for the given interface identifier.
func mockName(typeName string) string {
	return "Mock" + typeName
}

func (g *generator) GenerateMockInterface(iface *model.Interface, selfPackage string) {
	mockType := mockName(iface.Name)

	g.p("")
	g.p("// Mock of %v interface", iface.Name)
	g.p("type %v struct {", mockType)
	g.in().p("fail func(message string, callerSkip ...int)").out()
	g.p("}")
	g.p("")

	g.p("func New%v() *%v {", mockType, mockType)
	g.in().p("return &%v{fail: pegomock.GlobalFailHandler}", mockType).out()
	g.p("}")
	g.p("")

	for _, method := range iface.Methods {
		g.GenerateMockMethod(mockType, method, selfPackage).p("")
	}
	g.p("type Verifier%v struct {", iface.Name)
	g.in().
		p("mock *Mock%v", iface.Name).
		p("invocationCountMatcher pegomock.Matcher").
		p("inOrderContext *pegomock.InOrderContext").
		out()
	g.p("}")
	g.p("")
	g.p("func (mock *Mock%v) VerifyWasCalledOnce() *Verifier%v {", iface.Name, iface.Name)
	g.in().p("return &Verifier%v{mock, pegomock.Times(1), nil}", iface.Name).out()
	g.p("}")
	g.p("")
	g.p("func (mock *Mock%v) VerifyWasCalled(invocationCountMatcher pegomock.Matcher) *Verifier%v {", iface.Name, iface.Name)
	g.in().p("return &Verifier%v{mock, invocationCountMatcher, nil}", iface.Name).out()
	g.p("}")
	g.p("")
	g.p("func (mock *Mock%v) VerifyWasCalledInOrder(invocationCountMatcher pegomock.Matcher, inOrderContext *pegomock.InOrderContext) *Verifier%v {", iface.Name, iface.Name)
	g.in().p("return &Verifier%v{mock, invocationCountMatcher, inOrderContext}", iface.Name).out()
	g.p("}")
	g.p("")
	for _, method := range iface.Methods {
		g.GenerateVerifierMethod(iface.Name, method, selfPackage).p("")
	}
}

// GenerateMockMethod generates a mock method implementation.
// If non-empty, pkgOverride is the package in which unqualified types reside.
func (g *generator) GenerateMockMethod(mockType string, method *model.Method, pkgOverride string) *generator {
	_, _, argString, rets, retString, callArgs := getStuff(method, g, pkgOverride)
	g.p("func (mock *%v) %v(%v)%v {", mockType, method.Name, argString, retString)
	g.in()
	r := ""
	if len(method.Out) > 0 {
		r = "result :="
	}
	g.p("%v pegomock.GetGenericMockFrom(mock).Invoke(\"%v\", %v)", r, method.Name, callArgs)
	if len(method.Out) > 0 {
		// TODO: translate LastInvocation into a Matcher so it can be used as key for Stubbings
		g.p("if len(result) == 0 {")
		g.in()
		retValues := make([]string, len(rets))
		for i, ret := range rets {
			g.p("var ret%v %v", i, ret)
			retValues[i] = fmt.Sprintf("ret%v", i)
		}
		g.p("return %v", strings.Join(retValues, ", "))
		g.out()
		g.p("}")
		g.p("return %v", resultCast(rets))
	}
	g.out()
	g.p("}")
	return g
}

func resultCast(returnTypes []string) string {
	castedResults := make([]string, len(returnTypes))
	for i, returnType := range returnTypes {
		castedResults[i] = fmt.Sprintf("result[%v].(%v)", i, returnType)
	}
	return strings.Join(castedResults, ", ")
}

func (g *generator) GenerateVerifierMethod(interfaceName string, method *model.Method, pkgOverride string) *generator {
	_, _, argString, rets, retString, callArgs := getStuff(method, g, pkgOverride)

	g.p("func (verifier *Verifier%v) %v(%v)%v {", interfaceName, method.Name, argString, retString)
	g.p("pegomock.GetGenericMockFrom(verifier.mock).Verify(verifier.inOrderContext, verifier.invocationCountMatcher, \"%v\", %v)", method.Name, callArgs)

	if len(method.Out) > 0 {
		retValues := make([]string, len(rets))
		for i, ret := range rets {
			g.p("var ret%v %v", i, ret)
			retValues[i] = fmt.Sprintf("ret%v", i)
		}
		g.p("return %v", strings.Join(retValues, ", "))
	}
	g.p("}")

	return g
}

func getStuff(method *model.Method, g *generator, pkgOverride string) (
	args []string,
	argNames []string,
	argString string,
	rets []string,
	retString string,
	callArgs string,
) {
	args = make([]string, len(method.In))
	argNames = make([]string, len(method.In))
	for i, p := range method.In {
		name := p.Name
		if name == "" {
			name = fmt.Sprintf("_param%d", i)
		}
		ts := p.Type.String(g.packageMap, pkgOverride)
		args[i] = name + " " + ts
		argNames[i] = name
	}
	if method.Variadic != nil {
		name := method.Variadic.Name
		if name == "" {
			name = fmt.Sprintf("_param%d", len(method.In))
		}
		ts := method.Variadic.Type.String(g.packageMap, pkgOverride)
		args = append(args, name+" ..."+ts)
		argNames = append(argNames, name)
	}
	argString = strings.Join(args, ", ")

	rets = make([]string, len(method.Out))
	for i, p := range method.Out {
		rets[i] = p.Type.String(g.packageMap, pkgOverride)
	}
	retString = strings.Join(rets, ", ")
	if len(rets) > 1 {
		retString = "(" + retString + ")"
	}
	if retString != "" {
		retString = " " + retString
	}

	callArgs = strings.Join(argNames, ", ")
	// TODO: variadic arguments
	// if method.Variadic != nil {
	// 	// Non-trivial. The generated code must build a []interface{},
	// 	// but the variadic argument may be any type.
	// 	g.p("_s := []interface{}{%s}", strings.Join(argNames[:len(argNames)-1], ", "))
	// 	g.p("for _, _x := range %s {", argNames[len(argNames)-1])
	// 	g.in()
	// 	g.p("_s = append(_s, _x)")
	// 	g.out()
	// 	g.p("}")
	// 	callArgs = ", _s..."
	// }
	return
}

// Output returns the generator's output, formatted in the standard Go style.
func (g *generator) Output() []byte {
	src, err := format.Source(g.buf.Bytes())
	if err != nil {
		panic(fmt.Errorf("Failed to format generated source code: %s\n%s", err, g.buf.String()))
	}
	return src
}

func panicOnError(err error) {
	if err != nil {
		panic(err)
	}
}
