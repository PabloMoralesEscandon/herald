package pdf

// pageNode is one page with the attributes it inherited from the page tree.
type pageNode struct {
	dict      Dict
	resources Dict
	mediaBox  [4]float64
	rotate    int
}

// maxPageTreeDepth bounds a page tree that references itself.
const maxPageTreeDepth = 64

// pages walks the page tree in document order.
func (r *Reader) pages(limit int) []pageNode {
	catalog, ok := r.GetDict(r.trailer["Root"])
	if !ok {
		// Some damaged files have no usable /Root. Finding any page tree node
		// recovers them, and costs nothing when /Root is fine.
		catalog = r.findCatalog()
		if catalog == nil {
			return nil
		}
	}
	root, ok := r.GetDict(catalog["Pages"])
	if !ok {
		return nil
	}

	var collected []pageNode
	visited := map[string]bool{}
	inherited := pageNode{mediaBox: [4]float64{0, 0, 612, 792}}
	r.collectPages(root, inherited, visited, 0, limit, &collected)
	return collected
}

// findCatalog looks for a page tree when the trailer does not name one.
func (r *Reader) findCatalog() Dict {
	for number := range r.locations {
		dict, ok := r.GetDict(Ref{Number: number})
		if !ok {
			continue
		}
		if kind, _ := r.GetName(dict["Type"]); kind == "Catalog" {
			if _, ok := r.GetDict(dict["Pages"]); ok {
				return dict
			}
		}
	}
	return nil
}

func (r *Reader) collectPages(node Dict, inherited pageNode, visited map[string]bool, depth, limit int, out *[]pageNode) {
	if depth > maxPageTreeDepth || len(*out) >= limit {
		return
	}
	// Inheritable attributes are resolved on the way down.
	current := inherited
	if resources, ok := r.GetDict(node["Resources"]); ok {
		current.resources = resources
	}
	if box, ok := r.rectangle(node["MediaBox"]); ok {
		current.mediaBox = box
	}
	if rotate, ok := r.GetInt(node["Rotate"]); ok {
		current.rotate = ((rotate % 360) + 360) % 360
	}

	kind, _ := r.GetName(node["Type"])
	kids, hasKids := r.GetArray(node["Kids"])
	if kind == "Page" || (!hasKids && node["Contents"] != nil) {
		current.dict = node
		*out = append(*out, current)
		return
	}
	if !hasKids {
		return
	}
	for _, kid := range kids {
		if len(*out) >= limit {
			return
		}
		// A page tree that points back at an ancestor would loop forever.
		if ref, ok := kid.(Ref); ok {
			key := refKey(ref)
			if visited[key] {
				continue
			}
			visited[key] = true
		}
		child, ok := r.GetDict(kid)
		if !ok {
			continue
		}
		r.collectPages(child, current, visited, depth+1, limit, out)
	}
}

func refKey(ref Ref) string {
	return string(rune(ref.Number)) + ":" + string(rune(ref.Generation))
}

// rectangle reads a four-number array, normalizing the corner order.
func (r *Reader) rectangle(value Object) ([4]float64, bool) {
	array, ok := r.GetArray(value)
	if !ok || len(array) < 4 {
		return [4]float64{}, false
	}
	var box [4]float64
	for index := range 4 {
		number, ok := r.GetNumber(array[index])
		if !ok {
			return [4]float64{}, false
		}
		box[index] = number
	}
	if box[0] > box[2] {
		box[0], box[2] = box[2], box[0]
	}
	if box[1] > box[3] {
		box[1], box[3] = box[3], box[1]
	}
	if box[2]-box[0] <= 0 || box[3]-box[1] <= 0 {
		return [4]float64{}, false
	}
	return box, true
}

// contents concatenates a page's content streams.
//
// The specification allows a page's content to be split across an array of
// streams at arbitrary points, including in the middle of an operator, so they
// must be joined before tokenizing.
func (r *Reader) contents(page Dict) []byte {
	var joined []byte
	switch value := r.Resolve(page["Contents"]).(type) {
	case *Stream:
		data, err := r.Data(value)
		if err != nil {
			return nil
		}
		return data
	case Array:
		for _, item := range value {
			stream, ok := r.GetStream(item)
			if !ok {
				continue
			}
			data, err := r.Data(stream)
			if err != nil {
				continue
			}
			joined = append(joined, data...)
			joined = append(joined, '\n')
			if len(joined) > maxDecodedStream {
				return joined
			}
		}
	}
	return joined
}
