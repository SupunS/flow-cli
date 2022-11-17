/*
 * Flow CLI
 *
 * Copyright 2019 Dapper Labs, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *   http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package contracts

import (
	"fmt"
	"path"
	"strings"

	"github.com/onflow/cadence"

	"github.com/onflow/cadence/runtime/ast"
	"github.com/onflow/cadence/runtime/common"
	"github.com/onflow/cadence/runtime/parser"
	"github.com/onflow/flow-go-sdk"
	"gonum.org/v1/gonum/graph"
	"gonum.org/v1/gonum/graph/simple"
	"gonum.org/v1/gonum/graph/topo"
)

// Contract contains all the values a contract needs for deployment to the network.
//
// All the contract dependencies are defined here and later used when deploying on the network to
// define the order of deployments. We also define the account to which the contract needs to be deployed,
// and arguments used to deploy. Code contains replaced import statements with concrete addresses.
type Contract struct {
	index          int64
	location       string
	name           string
	accountAddress flow.Address
	accountName    string
	code           string
	args           []cadence.Value
	program        *ast.Program
	dependencies   map[string]*Contract
	aliases        map[string]flow.Address
}

func newContract(
	index int,
	location,
	code string,
	accountAddress flow.Address,
	accountName string,
	args []cadence.Value,
) (*Contract, error) {
	program, err := parser.ParseProgram([]byte(code), nil)
	if err != nil {
		return nil, err
	}

	if len(program.CompositeDeclarations())+len(program.InterfaceDeclarations()) != 1 {
		return nil, fmt.Errorf("the code must declare exactly one contract or contract interface")
	}

	return &Contract{
		index:          int64(index),
		location:       location,
		name:           parseName(program),
		accountAddress: accountAddress,
		accountName:    accountName,
		code:           code,
		program:        program,
		args:           args,
		dependencies:   make(map[string]*Contract),
		aliases:        make(map[string]flow.Address),
	}, nil
}

func (c *Contract) ID() int64 {
	return c.index
}

func (c *Contract) Name() string {
	return c.name
}

func (c *Contract) Location() string {
	return c.location
}

func (c *Contract) Code() string {
	return c.code
}

func (c *Contract) Args() []cadence.Value {
	return c.args
}

func (c *Contract) TranspiledCode() string {
	code := c.code

	for location, dep := range c.dependencies {
		code = strings.Replace(
			code,
			fmt.Sprintf(`"%s"`, location),
			fmt.Sprintf("0x%s", dep.Target()),
			1,
		)
	}

	for location, target := range c.aliases {
		code = strings.Replace(
			code,
			fmt.Sprintf(`"%s"`, location),
			fmt.Sprintf("0x%s", target),
			1,
		)
	}

	return code
}
func (c *Contract) AccountName() string {
	return c.accountName
}
func (c *Contract) Target() flow.Address {
	return c.accountAddress
}

func (c *Contract) Dependencies() map[string]*Contract {
	return c.dependencies
}

func (c *Contract) HasImports() bool {
	return len(c.imports()) > 0
}

func (c *Contract) imports() []string {
	imports := make([]string, 0)

	for _, imp := range c.program.ImportDeclarations() {
		location, ok := imp.Location.(common.StringLocation)
		if ok {
			imports = append(imports, location.String())
		}
	}

	return imports
}

func (c *Contract) addDependency(location string, dep *Contract) {
	c.dependencies[location] = dep
}

func (c *Contract) addAlias(location string, target flow.Address) {
	c.aliases[location] = target
}

func parseName(program *ast.Program) string {
	for _, compositeDeclaration := range program.CompositeDeclarations() {
		if compositeDeclaration.CompositeKind == common.CompositeKindContract {
			return compositeDeclaration.Identifier.Identifier
		}
	}

	for _, interfaceDeclaration := range program.InterfaceDeclarations() {
		if interfaceDeclaration.CompositeKind == common.CompositeKindContract {
			return interfaceDeclaration.Identifier.Identifier
		}
	}

	return ""
}

func absolutePath(basePath, relativePath string) string {
	return path.Join(path.Dir(basePath), relativePath)
}

// Deployments is a collection of contracts to deploy.
//
// Containing functionality to build a dependency tree between contracts and sort them based on that.
type Deployments struct {
	contracts           []*Contract
	loader              Loader
	aliases             map[string]string
	contractsByLocation map[string]*Contract
}

func NewDeployments(loader Loader, aliases map[string]string) *Deployments {
	return &Deployments{
		loader:              loader,
		aliases:             aliases,
		contractsByLocation: make(map[string]*Contract),
	}
}

func (c *Deployments) Contracts() []*Contract {
	return c.contracts
}

// Sort contracts by deployment order.
//
// Order of sorting is dependent on the possible imports contracts contains, since
// any imported contract must be deployed before deploying the contract with that import.
func (c *Deployments) Sort() error {
	err := c.ResolveImports()
	if err != nil {
		return err
	}

	sorted, err := sortByDeploymentOrder(c.contracts)
	if err != nil {
		return err
	}

	c.contracts = sorted
	return nil
}

func (c *Deployments) Add(
	location string,
	accountAddress flow.Address,
	accountName string,
	args []cadence.Value,
) (*Contract, error) {
	contractCode, err := c.loader.Load(location)
	if err != nil {
		return nil, err
	}

	contract, err := newContract(
		len(c.contracts),
		location,
		string(contractCode),
		accountAddress,
		accountName,
		args,
	)
	if err != nil {
		return nil, err
	}

	c.contracts = append(c.contracts, contract)
	c.contractsByLocation[contract.location] = contract

	return contract, nil
}

// ResolveImports checks every contract import and builds a dependency tree.
func (c *Deployments) ResolveImports() error {
	for _, contract := range c.contracts {
		for _, location := range contract.imports() {
			importPath := location // TODO: c.loader.Normalize(contract.source, source)
			importAlias, isAlias := c.aliases[importPath]
			importContract, isContract := c.contractsByLocation[importPath]

			if isContract {
				contract.addDependency(location, importContract)
			} else if isAlias {
				contract.addAlias(location, flow.HexToAddress(importAlias))
			} else {
				return fmt.Errorf("import from %s could not be found: %s, make sure import path is correct", contract.Name(), importPath)
			}
		}
	}

	return nil
}

// sortByDeploymentOrder sorts the given set of contracts in order of deployment.
//
// The resulting ordering ensures that each contract is deployed after all of its
// dependencies are deployed. This function returns an error if an import cycle exists.
//
// This function constructs a directed graph in which contracts are nodes and imports are edges.
// The ordering is computed by performing a topological sort on the constructed graph.
func sortByDeploymentOrder(contracts []*Contract) ([]*Contract, error) {
	g := simple.NewDirectedGraph()

	for _, c := range contracts {
		g.AddNode(c)
	}

	for _, c := range contracts {
		for _, dep := range c.dependencies {
			g.SetEdge(g.NewEdge(dep, c))
		}
	}

	sorted, err := topo.SortStabilized(g, nil)
	if err != nil {
		switch topoErr := err.(type) {
		case topo.Unorderable:
			return nil, &CyclicImportError{Cycles: nodeSetsToContractSets(topoErr)}
		default:
			return nil, err
		}
	}

	return nodesToContracts(sorted), nil
}

func nodeSetsToContractSets(nodes [][]graph.Node) [][]*Contract {
	contracts := make([][]*Contract, len(nodes))

	for i, s := range nodes {
		contracts[i] = nodesToContracts(s)
	}

	return contracts
}

func nodesToContracts(nodes []graph.Node) []*Contract {
	contracts := make([]*Contract, len(nodes))

	for i, s := range nodes {
		contracts[i] = s.(*Contract)
	}

	return contracts
}

// CyclicImportError is returned when contract contain cyclic imports one to the
// other which is not possible to be resolved and deployed.
type CyclicImportError struct {
	Cycles [][]*Contract
}

func (e *CyclicImportError) contractNames() [][]string {
	cycles := make([][]string, 0, len(e.Cycles))

	for _, cycle := range e.Cycles {
		contracts := make([]string, 0, len(cycle))
		for _, contract := range cycle {
			contracts = append(contracts, contract.Name())
		}

		cycles = append(cycles, contracts)
	}

	return cycles
}

func (e *CyclicImportError) Error() string {
	return fmt.Sprintf(
		"contracts: import cycle(s) detected: %v",
		e.contractNames(),
	)
}
