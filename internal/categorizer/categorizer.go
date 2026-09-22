package categorizer

import (
	"apimap/internal/linker"
)

type Stats struct {
	Total      int
	ByCategory map[linker.Category]int
	ByLinkType map[linker.LinkType]int
	ByClass    map[linker.URLClass]int
	ByTag      map[string]int
	Resolved   int
	Unresolved int
	WithParams int
}

func Calculate(links []linker.Link, showTags bool) *Stats {
	s := &Stats{
		Total:      len(links),
		ByCategory: make(map[linker.Category]int),
		ByLinkType: make(map[linker.LinkType]int),
		ByClass:    make(map[linker.URLClass]int),
		ByTag:      make(map[string]int),
	}
	for cat := linker.CategoryUnknown; cat <= linker.CategoryDynamic; cat++ {
		s.ByCategory[cat] = 0
	}
	for lt := linker.LinkTypeUnknown; lt <= linker.LinkTypeWeb; lt++ {
		s.ByLinkType[lt] = 0
	}
	for cls := linker.ClassNormal; cls <= linker.ClassNoise; cls++ {
		s.ByClass[cls] = 0
	}

	for _, link := range links {
		s.ByCategory[link.Category]++
		s.ByLinkType[link.LinkType]++
		s.ByClass[link.Class]++

		if len(link.ParamVariants) > 0 || link.HasParams {
			s.WithParams++
		}

		if link.HREF != "" {
			s.Unresolved++
		}
		if link.Resolved != "" {
			s.Resolved++
		}

		if showTags && link.Tag != "" {
			s.ByTag[link.Tag]++
		}
	}

	return s
}
