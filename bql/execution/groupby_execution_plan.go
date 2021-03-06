package execution

import (
	"fmt"
	"gopkg.in/sensorbee/sensorbee.v0/bql/udf"
	"gopkg.in/sensorbee/sensorbee.v0/core"
	"gopkg.in/sensorbee/sensorbee.v0/data"
)

type groupbyExecutionPlan struct {
	streamRelationStreamExecutionPlan
}

// tmpGroupData is an intermediate data structure to represent
// a set of rows that have the same values for GROUP BY columns.
type tmpGroupData struct {
	// this is the group (e.g. [1, "toy"]), where the values are
	// in order of the items in the GROUP BY clause
	group data.Array
	// for each aggregate function, we hold an array with the
	// input values.
	aggData map[string][]data.Value
	// as per our assumptions about grouping, the non-aggregation
	// data should be identical within every group
	nonAggData data.Map
}

// CanBuildGroupbyExecutionPlan checks whether the given statement
// allows to use an groupbyExecutionPlan.
func CanBuildGroupbyExecutionPlan(lp *LogicalPlan, reg udf.FunctionRegistry) bool {
	return lp.GroupingStmt
}

// NewGroupbyExecutionPlan builds a plan that follows the
// theoretical processing model. It supports only statements
// that use aggregation.
//
// After each tuple arrives,
// - compute the contents of the current window using the
//   specified window size/type,
// - perform a SELECT query on that data,
// - compute the data that need to be emitted by comparison with
//   the previous run's results.
func NewGroupbyExecutionPlan(lp *LogicalPlan, reg udf.FunctionRegistry) (PhysicalPlan, error) {
	underlying, err := newStreamRelationStreamExecutionPlan(lp, reg)
	if err != nil {
		return nil, err
	}
	return &groupbyExecutionPlan{
		*underlying,
	}, nil
}

// Process takes an input tuple and returns a slice of Map values that
// correspond to the results of the query represented by this execution
// plan. Note that the order of items in the returned slice is undefined
// and cannot be relied on.
func (ep *groupbyExecutionPlan) Process(input *core.Tuple) ([]data.Map, error) {
	return ep.process(input, ep.performQueryOnBuffer)
}

// performQueryOnBuffer computes the projections of a SELECT query on the data
// stored in `ep.filteredInputRows`. The query results (which is a set of
// data.Value, not core.Tuple) is stored in ep.curResults. The data
// that was stored in ep.curResults before this method was called is
// moved to ep.prevResults. Note that the order of values in ep.curResults
// is undefined.
//
// In case of an error the contents of ep.curResults will still be
// the same as before the call (so that the next run performs as
// if no error had happened), but the contents of ep.curResults are
// undefined.
func (ep *groupbyExecutionPlan) performQueryOnBuffer() error {
	// reuse the allocated memory
	output := ep.prevResults[0:0]
	// remember the previous results
	ep.prevResults = ep.curResults

	rollback := func() {
		// NB. ep.prevResults currently points to an slice with
		//     results from the previous run. ep.curResults points
		//     to the same slice. output points to a different slice
		//     with a different underlying array.
		//     in the next run, output will be reusing the underlying
		//     storage of the current ep.prevResults to hold results.
		//     therefore when we leave this function we must make
		//     sure that ep.prevResults and ep.curResults have
		//     different underlying arrays or ISTREAM/DSTREAM will
		//     return wrong results.
		ep.prevResults = output
	}

	// collect a list of all aggregate parameter evaluators in all
	// projections. this is necessary to avoid duplicate evaluation
	// if the same parameter is used in multiple aggregation funcs.
	allAggEvaluators := map[string]Evaluator{}
	for _, proj := range ep.projections {
		for key, agg := range proj.aggrEvals {
			allAggEvaluators[key] = agg
		}
	}

	// groups holds one item for every combination of values that
	// appear in the GROUP BY clause
	groups := map[data.HashValue][]*tmpGroupData{}
	// we also keep a list of group keys so that we can still loop
	// over them in the order they were added
	groupKeys := []data.HashValue{}

	// findOrCreateGroup looks up the group that has the given
	// groupValues in the `groups`map. if there is no such
	// group, a new one is created and a copy of the given map
	// is used as a representative of this group's values.
	findOrCreateGroup := func(groupValues []data.Value, groupHash data.HashValue, nonGroupValues data.Map) (*tmpGroupData, error) {
		mkGroup := func() *tmpGroupData {
			newGroup := &tmpGroupData{
				// the values that make up this group
				groupValues,
				// the input values of the aggregate functions
				map[string][]data.Value{},
				// a representative set of values for this group for later evaluation
				// TODO actually we don't need the whole map,
				//      just the parts common to the whole group
				nonGroupValues.Copy(),
			}
			// initialize the map with the aggregate function inputs
			for _, proj := range ep.projections {
				for key := range proj.aggrEvals {
					newGroup.aggData[key] = make([]data.Value, 0, 1)
				}
			}
			return newGroup
		}

		// find the correct group
		groupCandidates, exists := groups[groupHash]
		var group *tmpGroupData
		// if there is no such group, create one
		if !exists {
			group = mkGroup()
			groups[groupHash] = []*tmpGroupData{group}
			groupKeys = append(groupKeys, groupHash)
		} else {
			// if we arrive here, there is a group with the same hash value
			// but we need to validate the data is actually the same
			for _, groupCandidate := range groupCandidates {
				if data.Equal(data.Array(groupValues), groupCandidate.group) {
					group = groupCandidate
					break
				}
			}
			// no group with the same groupValues was found, so create
			// one and append it to the list of groups with the same hash
			if group == nil {
				group = mkGroup()
				groups[groupHash] = append(groupCandidates, group)
			}
		}
		// return a pointer to the (found or created) group
		return group, nil
	}

	// function to compute the grouping expressions and store the
	// input for aggregate functions in the correct group.
	evalItem := func(io *inputRowWithCachedResult) error {
		var itemGroupValues data.Array
		// if we have a cached result, use this
		if io.cache != nil {
			cachedGroupValues, err := data.AsArray(io.cache)
			if err != nil {
				return fmt.Errorf("cached data was not an array: %v", io.cache)
			}
			itemGroupValues = cachedGroupValues
		} else {
			// otherwise, compute the expressions in the GROUP BY to find
			// the correct group to append to
			itemGroupValues = make([]data.Value, len(ep.groupList))
			for i, eval := range ep.groupList {
				// ordinary "flat" expression
				value, err := eval.Eval(*io.input)
				if err != nil {
					return err
				}
				itemGroupValues[i] = value
			}
			io.cache = itemGroupValues
			io.hash = data.Hash(io.cache)
		}

		itemGroup, err := findOrCreateGroup(itemGroupValues, io.hash, *io.input)
		if err != nil {
			return err
		}

		// now compute all the input data for the aggregate functions,
		// e.g. for `SELECT count(a) + max(b/2)`, compute `a` and `b/2`
		for key, agg := range allAggEvaluators {
			value, err := agg.Eval(*io.input)
			if err != nil {
				return err
			}
			// store this value in the output map
			itemGroup.aggData[key] = append(itemGroup.aggData[key], value)
		}
		return nil
	}

	evalGroup := func(group *tmpGroupData) error {
		result := data.Map(make(map[string]data.Value, len(ep.projections)))
		// collect input for aggregate functions into an array
		// within each group
		for key := range allAggEvaluators {
			group.nonAggData[key] = data.Array(group.aggData[key])
			delete(group.aggData, key)
		}
		// evaluate HAVING condition, if there is one
		for _, proj := range ep.projections {
			if proj.alias == ":having:" {
				havingResult, err := proj.evaluator.Eval(group.nonAggData)
				if err != nil {
					return err
				}
				// a NULL value is definitely not "true", so since we
				// have only a binary decision, we should drop tuples
				// where the condition evaluates to NULL
				havingResultBool := false
				if havingResult.Type() != data.TypeNull {
					havingResultBool, err = data.AsBool(havingResult)
					if err != nil {
						return err
					}
				}
				// if it evaluated to false, do not further process this group
				if !havingResultBool {
					return nil
				}
				break
			}
		}
		// now evaluate all other projections
		for _, proj := range ep.projections {
			if proj.alias == ":having:" {
				continue
			}
			// now evaluate this projection on the flattened data
			value, err := proj.evaluator.Eval(group.nonAggData)
			if err != nil {
				return err
			}
			if err := assignOutputValue(result, proj.alias, proj.aliasPath, value); err != nil {
				return err
			}
		}
		output = append(output, resultRow{row: result, hash: data.Hash(result)})
		return nil
	}

	evalNoGroup := func() error {
		// if we have an empty group list *and* a GROUP BY clause,
		// we have to return an empty result (because there are no
		// rows with "the same values"). but if the list is empty and
		// we *don't* have a GROUP BY clause, then we need to compute
		// all foldables and aggregates with an empty input
		if len(ep.groupList) > 0 {
			return nil
		}
		input := data.Map{}
		result := data.Map(make(map[string]data.Value, len(ep.projections)))
		for _, proj := range ep.projections {
			// collect input for aggregate functions
			if proj.hasAggregate {
				for key := range proj.aggrEvals {
					input[key] = data.Array{}
				}
			}
			// now evaluate this projection on the flattened data.
			// note that input has *only* the keys of the empty
			// arrays, no other columns, but we cannot have other
			// columns involved in the projection (since we know
			// that GROUP BY is empty).
			value, err := proj.evaluator.Eval(input)
			if err != nil {
				return err
			}
			if err := assignOutputValue(result, proj.alias, proj.aliasPath, value); err != nil {
				return err
			}
		}
		output = append(output, resultRow{row: result, hash: data.Hash(result)})
		return nil
	}

	// compute the output for each item in ep.filteredInputRows
	for e := ep.filteredInputRows.Front(); e != nil; e = e.Next() {
		item := e.Value.(*inputRowWithCachedResult)
		if err := evalItem(item); err != nil {
			rollback()
			return err
		}
	}

	// if we arrive here, then the input for the aggregation functions
	// is in the `group` list and we need to compute aggregation and output.
	// NB. we do not directly loop over the `groups` map to avoid random order.
	for _, groupKey := range groupKeys {
		groupsWithSameHash := groups[groupKey]
		for _, group := range groupsWithSameHash {
			if err := evalGroup(group); err != nil {
				rollback()
				return err
			}
		}
	}
	if len(groups) == 0 {
		if err := evalNoGroup(); err != nil {
			rollback()
			return err
		}
	}

	ep.curResults = output
	return nil
}
