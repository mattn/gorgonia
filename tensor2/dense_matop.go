package tensor

import "github.com/pkg/errors"

type Mapper interface {
	Map(Array) error
}

// // Apply applies a function to all the values in the ndarray
// func (t *Dense) Apply(fn func(float64) float64, opts ...FuncOpt) (retVal *Dense, err error) {
// 	safe, incr, reuse := parseSafeReuse(opts...)

// 	// check reuse and stuff
// 	var res []float64
// 	switch {
// 	case reuse != nil:
// 		res = reuse.data
// 		if len(res) != t.Size() {
// 			err = shapeMismatchError(t.Shape(), reuse.Shape())
// 			return
// 		}
// 	case !safe:
// 		res = t.data
// 	default:
// 		if t.IsMaterializable() {
// 			res = make([]float64, t.Shape().TotalSize())

// 		} else {
// 			res = make([]float64, len(t.data))
// 		}
// 	}
// 	// do
// 	switch {
// 	case t.viewOf == nil && !incr:
// 		for i, v := range t.data {
// 			res[i] = fn(v)
// 		}
// 	case t.viewOf == nil && incr:
// 		for i, v := range t.data {
// 			res[i] += fn(v)
// 		}
// 	case t.viewOf != nil && !incr:
// 		it := NewFlatIterator(t.AP)
// 		var next, i int
// 		for next, err = it.Next(); err == nil; next, err = it.Next() {
// 			if _, noop := err.(NoOpError); err != nil && !noop {
// 				return
// 			}
// 			res[i] = fn(t.data[next])
// 			i++
// 		}
// 		err = nil
// 	case t.viewOf != nil && incr:
// 		it := NewFlatIterator(t.AP)
// 		var next, i int
// 		for next, err = it.Next(); err == nil; next, err = it.Next() {
// 			if _, noop := err.(NoOpError); err != nil && !noop {
// 				return
// 			}

// 			res[i] += fn(t.data[next])
// 			i++
// 		}
// 		err = nil
// 	default:
// 		err = notyetimplemented("Apply not implemented for this state: isView: %t and incr: %t", t.viewOf == nil, incr)
// 		return
// 	}
// 	// set retVal
// 	switch {
// 	case reuse != nil:
// 		if err = reuseCheckShape(reuse, t.Shape()); err != nil {
// 			return
// 		}
// 		retVal = reuse
// 	case !safe:
// 		retVal = t
// 	default:
// 		retVal = NewTensor(WithBacking(res), WithShape(t.Shape()...))
// 	}
// 	return
// }

// T performs a thunked transpose. It doesn't actually do anything, except store extra information about the post-transposed shapes and strides
// Usually this is more than enough, as BLAS will handle the rest of the transpose
func (t *Dense) T(axes ...int) (err error) {
	var transform *AP
	if transform, axes, err = t.AP.T(axes...); err != nil {
		if _, ok := err.(NoOpError); !ok {
			return
		}
		err = nil
		return
	}

	// is there any old transposes that need to be done first?
	// this is important, because any old transposes for dim >=3 are merely permutations of the strides
	if t.old != nil {
		if t.IsVector() {
			// the transform that was calculated was a waste of time - return it to the pool then untranspose
			ReturnAP(transform)
			t.UT()
			return
		}

		// check if the current axes are just a reverse of the previous transpose's
		isReversed := true
		for i, s := range t.oshape() {
			if transform.Shape()[i] != s {
				isReversed = false
				break
			}
		}

		// if it is reversed, well, we just restore the backed up one
		if isReversed {
			ReturnAP(transform)
			t.UT()
			return
		}

		// cool beans. No funny reversals. We'd have to actually do transpose then
		t.Transpose()
	}

	// swap out the old and the new
	t.old = t.AP
	t.transposeWith = axes
	t.AP = transform
	return nil
}

// UT is a quick way to untranspose a currently transposed *Dense
// The reason for having this is quite simply illustrated by this problem:
//		T = NewTensor(WithShape(2,3,4))
//		T.T(1,2,0)
//
// To untranspose that, we'd need to apply a transpose of (2,0,1).
// This means having to keep track and calculate the transposes.
// Instead, here's a helpful convenience function to instantly untranspose any previous transposes.
//
// Nothing will happen if there was no previous transpose
func (t *Dense) UT() {
	if t.old != nil {
		ReturnAP(t.AP)
		ReturnInts(t.transposeWith)
		t.AP = t.old
		t.old = nil
		t.transposeWith = nil
	}
}

// SafeT is exactly like T(), except it returns a new *Dense. The data is also copied over, unmoved.
func (t *Dense) SafeT(axes ...int) (retVal *Dense, err error) {
	var transform *AP
	if transform, axes, err = t.AP.T(axes...); err != nil {
		if _, ok := err.(NoOpError); !ok {
			return
		}
		err = nil
		return
	}

	retVal = newTypedShapedDense(t.t, Shape{t.data.Len()})
	if _, err = copyArray(retVal.data, t.data); err != nil {
		return
	}

	retVal.AP = transform
	retVal.old = t.AP.Clone()
	retVal.transposeWith = axes

	return
}

// Transpose() actually transposes the data.
// This is a generalized version of the inplace matrix transposition algorithm from Wikipedia:
// https://en.wikipedia.org/wiki/In-place_matrix_transposition
func (t *Dense) Transpose() {
	// if there is no oldinfo, that means the current info is the latest, and not the transpose
	if t.old == nil {
		return
	}

	if t.IsScalar() {
		return // cannot transpose scalars
	}

	defer func() {
		ReturnAP(t.old)
		t.old = nil
		t.transposeWith = nil
	}()

	expShape := t.Shape()
	expStrides := expShape.CalcStrides() // important! because the strides would have changed once the underlying data changed
	defer ReturnInts(expStrides)

	size := t.data.Len()
	axes := t.transposeWith

	if t.IsVector() {
		t.setShape(expShape...)
		// no change of strides.
		return
	}

	// here we'll create a bit-map -- 64 bits should be more than enough
	// (I don't expect to be dealing with matrices that are larger than 64 elements that requires transposes to be done)
	//
	// The purpose of the bit-map is to track which elements have been moved to their correct places
	//
	// To set ith bit: track |= (1 << i)
	// To check if ith bit is set: track & (1 << i)
	// To check every bit up to size is unset: (1 << size)
	//

	track := NewBitMap(size)
	track.Set(0)
	track.Set(size - 1) // first and last don't change

	// // we start our iteration at 1, because transposing 0 does noting.
	var saved, tmp interface{}
	var i int

	for i = 1; ; {
		dest := t.transposeIndex(i, axes, expStrides)

		if track.IsSet(i) && track.IsSet(dest) {
			t.data.Set(i, saved)
			saved = t.t.ZeroValue()

			for i < size && track.IsSet(i) {
				i++
			}

			if i >= size {
				break
			}
			continue
		}

		track.Set(i)
		tmp = t.data.Get(i)
		t.data.Set(i, saved)
		saved = tmp

		i = dest
	}

	t.setShape(expShape...)
	t.sanity()
}

// At returns the value at the given coordinate
func (t *Dense) At(coords ...int) (interface{}, error) {
	if len(coords) != t.Dims() {
		return nil, errors.Errorf(shapeMismatch, len(coords), t.Dims())
	}

	at, err := t.at(coords...)
	if err != nil {
		return nil, errors.Wrap(err, "At()")
	}

	return t.data.Get(at), nil
}

// Repeat is like Numpy's repeat. It repeats the elements of an array.
// The repeats param defines how many times each element in the axis is repeated.
// Just like NumPy, the repeats param is broadcasted to fit the size of the given axis.
func (t *Dense) Repeat(axis int, repeats ...int) (retVal Tensor, err error) {
	var newShape Shape
	var size int
	if newShape, repeats, size, err = t.Shape().Repeat(axis, repeats...); err != nil {
		return nil, errors.Wrap(err, "Unable to get repeated shape")
	}

	if axis == AllAxes {
		axis = 0
	}

	d := newTypedShapedDense(t.t, newShape)

	var outers int
	if t.IsScalar() {
		outers = 1
	} else {
		outers = ProdInts(t.Shape()[0:axis])
		if outers == 0 {
			outers = 1
		}
	}

	var stride, newStride int
	if newShape.IsVector() {
		stride = 1 // special case
	} else if t.IsVector() {
		stride = 1 // special case because CalcStrides() will return []int{1} as the strides for a vector
	} else {
		stride = t.ostrides()[axis]
	}

	if newShape.IsVector() {
		newStride = 1
	} else {
		newStride = d.ostrides()[axis]
	}

	var destStart, srcStart int
	for i := 0; i < outers; i++ {
		for j := 0; j < size; j++ {
			var tmp int
			tmp = repeats[j]

			for k := 0; k < tmp; k++ {
				if srcStart >= t.data.Len() || destStart+stride > d.data.Len() {
					break
				}

				if _, err = copySlicedArray(d.data, destStart, d.data.Len(), t.data, srcStart, t.data.Len()); err != nil {
					return nil, errors.Wrap(err, "Failed to Repeat()")
				}

				destStart += newStride
			}
			srcStart += stride
		}
	}

	return d, nil
}

// CopyTo copies the underlying data to the destination *Dense. The original data is untouched.
// Note: CopyTo doesn't care about the metadata of the destination *Dense. Take for example:
//		T = NewTensor(WithShape(6))
//		T2 = NewTensor(WithShape(2,3))
//		err = T.CopyTo(T2) // err == nil
//
// The only time that this will fail is if the underlying sizes are different
func (t *Dense) CopyTo(other *Dense) error {
	if other == t {
		return nil // nothing to copy to. Maybe return NoOpErr?
	}

	if other.Size() != t.Size() {
		return errors.Errorf(sizeMismatch, t.Size(), other.Size())
	}

	// easy peasy lemon squeezy
	if t.viewOf == nil && other.viewOf == nil {
		_, err := copyArray(other.data, t.data)
		return err
	}

	return errors.Errorf(methodNYI, "CopyTo", "views")
}

// Slice performs slicing on the ndarrays. It returns a view which shares the same underlying memory as the original ndarray.
// In the original design, views are read-only. However, as things have changed, views are now mutable.
//
// Example. Given:
//		T = NewTensor(WithShape(2,2), WithBacking(RangeFloat64(0,4)))
//		V, _ := T.Slice(nil, singleSlice(1)) // T[:, 1]
//
// Any modification to the values in V, will be reflected in T as well.
//
// The method treats <nil> as equivalent to a colon slice. T.Slice(nil) is equivalent to T[:] in Numpy syntax
func (t *Dense) Slice(slices ...Slice) (view *Dense, err error) {
	var newAP *AP
	var ndStart, ndEnd int
	if newAP, ndStart, ndEnd, err = t.AP.S(t.data.Len(), slices...); err != nil {
		return
	}

	view = new(Dense)
	view.t = t.t
	view.viewOf = t
	view.AP = newAP
	view.data, err = sliceArray(t.data, ndStart, ndEnd)
	return
}

/* Private Methods */

// returns the new index given the old index
func (t *Dense) transposeIndex(i int, transposePat, strides []int) int {
	oldCoord, err := Itol(i, t.oshape(), t.ostrides())
	if err != nil {
		panic(err)
	}

	/*
		coordss, _ := Permute(transposePat, oldCoord)
		coords := coordss[0]
		expShape := t.Shape()
		index, _ := Ltoi(expShape, strides, coords...)
	*/

	// The above is the "conceptual" algorithm.
	// Too many checks above slows things down, so the below is the "optimized" edition
	var index int
	for i, axis := range transposePat {
		index += oldCoord[axis] * strides[i]
	}
	return index
}

// at returns the index at which the coordinate is refering to.
// This function encapsulates the addressing of elements in a contiguous block.
// For a 2D ndarray, ndarray.at(i,j) is
//		at = ndarray.strides[0]*i + ndarray.strides[1]*j
// This is of course, extensible to any number of dimensions.
func (t *Dense) at(coords ...int) (at int, err error) {
	return Ltoi(t.Shape(), t.Strides(), coords...)
}

// iToCoord is the inverse function of at().
func (t *Dense) itol(i int) (coords []int, err error) {
	var oShape Shape
	var oStrides []int

	if t.old != nil {
		oShape = t.old.Shape()
		oStrides = t.old.Strides()
	} else {
		oShape = t.Shape()
		oStrides = t.Strides()
	}

	// use the original shape, permute the coordinates later
	if coords, err = Itol(i, oShape, oStrides); err != nil {
		err = errors.Wrapf(err, "Failed to do Itol with i: %d, oShape: %v; oStrides: %v", i, oShape, oStrides)
		return
	}

	if t.transposeWith != nil {
		var res [][]int
		if res, err = Permute(t.transposeWith, coords); err == nil {
			coords = res[0]
		}
	}
	return
}

// RollAxis rolls the axis backwards until it lies in the given position.
//
// This method was adapted from Numpy's Rollaxis. The licence for Numpy is a BSD-like licence and can be found here: https://github.com/numpy/numpy/blob/master/LICENSE.txt
//
// As a result of being adapted from Numpy, the quirks are also adapted. A good guide reducing the confusion around rollaxis can be found here: http://stackoverflow.com/questions/29891583/reason-why-numpy-rollaxis-is-so-confusing (see answer by hpaulj)
func (t *Dense) RollAxis(axis, start int, safe bool) (retVal *Dense, err error) {
	dims := t.Dims()

	if !(axis >= 0 && axis < dims) {
		err = errors.Errorf(invalidAxis, axis, dims)
		return
	}

	if !(start >= 0 && start <= dims) {
		err = errors.Wrap(errors.Errorf(invalidAxis, axis, dims), "Start axis is wrong")
		return
	}

	if axis < start {
		start--
	}

	if axis == start {
		retVal = t
		return
	}

	axes := BorrowInts(dims)
	defer ReturnInts(axes)

	for i := 0; i < dims; i++ {
		axes[i] = i
	}
	copy(axes[axis:], axes[axis+1:])
	copy(axes[start+1:], axes[start:])
	axes[start] = axis

	if safe {
		return t.SafeT(axes...)
	}
	err = t.T(axes...)
	retVal = t
	return
}

// Concat concatenates the other tensors along the given axis. It is like Numpy's concatenate() function.
func (t *Dense) Concat(axis int, Ts ...*Dense) (retVal *Dense, err error) {
	ss := make([]Shape, len(Ts))
	for i, T := range Ts {
		ss[i] = T.Shape()
	}
	var newShape Shape
	if newShape, err = t.Shape().Concat(axis, ss...); err != nil {
		return
	}

	newStrides := newShape.CalcStrides()
	data := makeArray(t.t, newShape.TotalSize())

	retVal = new(Dense)
	retVal.t = t.t
	retVal.AP = NewAP(newShape, newStrides)
	retVal.data = data

	all := make([]*Dense, len(Ts)+1)
	all[0] = t
	copy(all[1:], Ts)

	// special case
	var start, end int

	for _, T := range all {
		end += T.Shape()[axis]
		slices := make([]Slice, axis+1)
		slices[axis] = makeRS(start, end)

		var v *Dense
		if v, err = retVal.Slice(slices...); err != nil {
			return
		}
		if err = assignArray(v, T); err != nil {
			return
		}
		start = end
	}

	return
}

// Stack stacks the other tensors along the axis specified. It is like Numpy's stack function.
func (t *Dense) Stack(axis int, others ...*Dense) (retVal *Dense, err error) {
	opdims := t.Dims()
	if axis >= opdims+1 {
		err = errors.Errorf(dimMismatch, opdims+1, axis)
		return
	}

	newShape := Shape(BorrowInts(opdims + 1))
	newShape[axis] = len(others) + 1
	shape := t.Shape()
	var cur int
	for i, s := range shape {
		if i == axis {
			cur++
		}
		newShape[cur] = s
		cur++
	}

	newStrides := newShape.CalcStrides()
	ap := NewAP(newShape, newStrides)

	allNoMat := !t.IsMaterializable()
	for _, ot := range others {
		if allNoMat && ot.IsMaterializable() {
			allNoMat = false
		}
	}

	var data Array

	// the "viewStack" method is the more generalized method
	// and will work for all Tensors, regardless of whether it's a view
	// But the simpleStack is faster, and is an optimization
	if allNoMat {
		data = t.simpleStack(axis, ap, others...)
	} else {
		data = t.viewStack(axis, ap, others...)
	}

	retVal = new(Dense)
	retVal.t = t.t
	retVal.AP = ap
	retVal.data = data
	return
}

// simpleStack is the data movement function for non-view tensors. What it does is simply copy the data according to the new strides
func (t *Dense) simpleStack(axis int, ap *AP, others ...*Dense) (data Array) {
	data = makeArray(t.t, ap.Size())

	switch axis {
	case 0:
		copyArray(data, t.data)
		next := t.data.Len()
		for _, ot := range others {
			copySlicedArray(data, next, data.Len(), ot.data, 0, ot.data.Len())
			next += ot.data.Len()
		}
	default:
		axisStride := ap.Strides()[axis]
		batches := data.Len() / axisStride

		destStart := 0
		start := 0
		end := start + axisStride

		for i := 0; i < batches; i++ {
			copySlicedArray(data, destStart, data.Len(), t.data, start, end)
			for _, ot := range others {
				destStart += axisStride
				copySlicedArray(data, destStart, data.Len(), ot.data, start, end)
				i++
			}
			destStart += axisStride
			start += axisStride
			end += axisStride
		}
	}
	return
}

// viewStack is the data movement function for Stack(), applied on views
func (t *Dense) viewStack(axis int, ap *AP, others ...*Dense) Array {
	// data = makeArray(t.t, ap.Size())
	data := make([]interface{}, ap.Size())
	axisStride := ap.Strides()[axis]
	batches := len(data) / axisStride

	it := NewFlatIterator(t.AP)
	ch := it.Chan()
	chs := make([]chan int, len(others))
	chs = chs[:0]
	for _, ot := range others {
		oter := NewFlatIterator(ot.AP)
		chs = append(chs, oter.Chan())
	}

	data = data[:0]
	for i := 0; i < batches; i++ {
		for j := 0; j < axisStride; j++ {
			id, ok := <-ch
			if !ok {
				break
			}
			data = append(data, t.data.Get(id))
		}
		for j, ot := range others {
			for k := 0; k < axisStride; k++ {
				id, ok := <-chs[j]
				if !ok {
					break
				}
				data = append(data, ot.data.Get(id))
			}
		}
	}
	return fromInterfaceSlice(t.t, data)
}