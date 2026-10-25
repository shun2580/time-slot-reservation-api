package handler

import "sort"

func sortFieldErrors(errs []FieldError) {
	sort.Slice(errs, func(i, j int) bool { return errs[i].Field < errs[j].Field })
}
