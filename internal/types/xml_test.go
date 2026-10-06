package types_test

import (
	"testing"

	"github.com/Deln0r/ygo/internal/doc"
	"github.com/Deln0r/ygo/internal/encoding"
	"github.com/Deln0r/ygo/internal/types"
)

func TestXml_Fragment_InsertElement_RoundTrip(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 100})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	p := frag.InsertXmlElement(txn, 0, "p")
	if p == nil {
		t.Fatal("InsertXmlElement returned nil")
	}
	if p.NodeName() != "p" {
		t.Errorf("NodeName = %q, want p", p.NodeName())
	}
	txn.Commit()

	if got, want := frag.Length(), uint64(1); got != want {
		t.Errorf("Length = %d, want %d", got, want)
	}
	child := frag.Get(0)
	pBack, ok := child.(*types.XmlElement)
	if !ok {
		t.Fatalf("Get(0) returned %T, want *types.XmlElement", child)
	}
	if pBack.NodeName() != "p" {
		t.Errorf("Get(0).NodeName = %q, want p", pBack.NodeName())
	}
}

func TestXml_Element_Attributes(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 101})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	div := frag.InsertXmlElement(txn, 0, "div")
	div.SetAttribute(txn, "class", "container")
	div.SetAttribute(txn, "id", "main")
	txn.Commit()

	if v, ok := div.GetAttribute("class"); !ok || v != "container" {
		t.Errorf("GetAttribute(class) = (%q, %v), want (container, true)", v, ok)
	}
	if v, ok := div.GetAttribute("id"); !ok || v != "main" {
		t.Errorf("GetAttribute(id) = (%q, %v), want (main, true)", v, ok)
	}
	if _, ok := div.GetAttribute("missing"); ok {
		t.Error("GetAttribute(missing) reported present")
	}

	attrs := div.Attributes()
	if len(attrs) != 2 || attrs["class"] != "container" || attrs["id"] != "main" {
		t.Errorf("Attributes = %v, want {class:container, id:main}", attrs)
	}

	// Attributes do not contribute to child Length.
	if div.Length() != 0 {
		t.Errorf("Length after attr set = %d, want 0 (attrs are map-keyed)", div.Length())
	}
}

func TestXml_Element_RemoveAttribute(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 102})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	span := frag.InsertXmlElement(txn, 0, "span")
	span.SetAttribute(txn, "data-x", "1")
	span.SetAttribute(txn, "data-y", "2")
	txn.Commit()

	txn = d.WriteTxn()
	span.RemoveAttribute(txn, "data-x")
	txn.Commit()

	if _, ok := span.GetAttribute("data-x"); ok {
		t.Error("data-x still present after remove")
	}
	if v, ok := span.GetAttribute("data-y"); !ok || v != "2" {
		t.Errorf("data-y = (%q, %v), want (2, true)", v, ok)
	}
}

func TestXml_Element_Children_NestedDOM(t *testing.T) {
	// <p><strong>bold</strong> normal</p>
	d := doc.NewDocWithOptions(doc.Options{ClientID: 103})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	p := frag.InsertXmlElement(txn, 0, "p")

	strong := p.InsertXmlElement(txn, 0, "strong")
	strongText := strong.InsertXmlText(txn, 0)
	_ = strongText.Insert(txn, 0, "bold")

	plain := p.InsertXmlText(txn, 1)
	_ = plain.Insert(txn, 0, " normal")
	txn.Commit()

	if got, want := p.Length(), uint64(2); got != want {
		t.Errorf("p.Length = %d, want %d", got, want)
	}

	got := frag.ToString()
	want := "<p><strong>bold</strong> normal</p>"
	if got != want {
		t.Errorf("ToString = %q, want %q", got, want)
	}
}

// An empty element is written with a closing tag, never self-closing,
// and the tag in lower case, as yjs YXmlElement.toString writes it.
func TestXml_Element_ToString_EmptyElementAsYjs(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 104})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	br := frag.InsertXmlElement(txn, 0, "br")
	br.SetAttribute(txn, "class", "clearfix")
	txn.Commit()

	got := frag.ToString()
	want := `<br class="clearfix"></br>`
	if got != want {
		t.Errorf("ToString = %q, want %q", got, want)
	}
}

func TestXml_Text_RichFormatting(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 105})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	txt := frag.InsertXmlText(txn, 0)
	_ = txt.InsertWithAttributes(txn, 0, "bold", types.Attrs{"bold": true})
	_ = txt.Insert(txn, 4, " plain")
	txn.Commit()

	if got, want := txt.String(), "bold plain"; got != want {
		t.Errorf("XmlText.String = %q, want %q", got, want)
	}

	delta := txt.ToDelta()
	if len(delta) != 2 {
		t.Fatalf("delta len = %d, want 2; got %+v", len(delta), delta)
	}
	if delta[0].Insert != "bold" || delta[0].Attributes["bold"] != true {
		t.Errorf("delta[0] = %+v", delta[0])
	}
	if delta[1].Insert != " plain" {
		t.Errorf("delta[1] = %+v", delta[1])
	}
}

func TestXml_WireRoundTrip_PreservesStructure(t *testing.T) {
	src := doc.NewDocWithOptions(doc.Options{ClientID: 200})
	frag := types.NewXmlFragment(src.Branch("page"))

	txn := src.WriteTxn()
	h1 := frag.InsertXmlElement(txn, 0, "h1")
	h1.SetAttribute(txn, "class", "title")
	h1Text := h1.InsertXmlText(txn, 0)
	_ = h1Text.Insert(txn, 0, "Hello")

	p := frag.InsertXmlElement(txn, 1, "p")
	pText := p.InsertXmlText(txn, 0)
	_ = pText.InsertWithAttributes(txn, 0, "world", types.Attrs{"em": true})
	txn.Commit()

	expected := frag.ToString()

	update := encoding.EncodeStateAsUpdate(src)

	target := doc.NewDoc()
	if err := encoding.ApplyUpdate(target, update); err != nil {
		t.Fatal(err)
	}
	targetFrag := types.NewXmlFragment(target.Branch("page"))
	if got := targetFrag.ToString(); got != expected {
		t.Errorf("after wire round-trip:\n got  %q\n want %q", got, expected)
	}
}

func TestXml_CrossClient_StructuralEditConverges(t *testing.T) {
	// Client A creates <p>text</p>. Client B observes A, then
	// inserts <em>!</em> at end of p. Both converge.
	a := doc.NewDocWithOptions(doc.Options{ClientID: 300})
	aFrag := types.NewXmlFragment(a.Branch("page"))
	t1 := a.WriteTxn()
	aP := aFrag.InsertXmlElement(t1, 0, "p")
	aText := aP.InsertXmlText(t1, 0)
	_ = aText.Insert(t1, 0, "hello")
	t1.Commit()

	// B observes A.
	b := doc.NewDocWithOptions(doc.Options{ClientID: 301})
	if err := encoding.ApplyUpdate(b, encoding.EncodeStateAsUpdate(a)); err != nil {
		t.Fatal(err)
	}
	bFrag := types.NewXmlFragment(b.Branch("page"))
	bP, ok := bFrag.Get(0).(*types.XmlElement)
	if !ok {
		t.Fatal("B's first child not XmlElement")
	}
	// B adds <em>!</em> after the text node.
	t2 := b.WriteTxn()
	em := bP.InsertXmlElement(t2, bP.Length(), "em")
	emText := em.InsertXmlText(t2, 0)
	_ = emText.Insert(t2, 0, "!")
	t2.Commit()

	// A receives B's update.
	if err := encoding.ApplyUpdate(a, encoding.EncodeStateAsUpdate(b)); err != nil {
		t.Fatal(err)
	}

	want := "<p>hello<em>!</em></p>"
	for label, frag := range map[string]*types.XmlFragment{"A": aFrag, "B": bFrag} {
		if got := frag.ToString(); got != want {
			t.Errorf("%s ToString = %q, want %q", label, got, want)
		}
	}
}

func TestXml_Fragment_DeleteChild(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 400})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	a := frag.InsertXmlElement(txn, 0, "a")
	b := frag.InsertXmlElement(txn, 1, "b")
	_ = a
	_ = b
	txn.Commit()
	if got := frag.Length(); got != 2 {
		t.Fatalf("Length = %d, want 2", got)
	}

	txn = d.WriteTxn()
	frag.Delete(txn, 0, 1)
	txn.Commit()

	if got := frag.Length(); got != 1 {
		t.Errorf("post-delete Length = %d, want 1", got)
	}
	rem, ok := frag.Get(0).(*types.XmlElement)
	if !ok || rem.NodeName() != "b" {
		t.Errorf("post-delete Get(0).NodeName = %v, want b", rem)
	}
}

func TestXml_Range_VisitsChildrenInOrder(t *testing.T) {
	d := doc.NewDocWithOptions(doc.Options{ClientID: 500})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	frag.InsertXmlElement(txn, 0, "h1")
	frag.InsertXmlElement(txn, 1, "p")
	frag.InsertXmlElement(txn, 2, "footer")
	txn.Commit()

	var names []string
	frag.Range(func(_ uint64, child any) bool {
		if el, ok := child.(*types.XmlElement); ok {
			names = append(names, el.NodeName())
		}
		return true
	})
	if len(names) != 3 || names[0] != "h1" || names[1] != "p" || names[2] != "footer" {
		t.Errorf("Range names = %v, want [h1 p footer]", names)
	}
}

func TestXml_XmlText_IsRichText(t *testing.T) {
	// XmlText embeds Text — every rich-text method works.
	d := doc.NewDocWithOptions(doc.Options{ClientID: 600})
	frag := types.NewXmlFragment(d.Branch("page"))

	txn := d.WriteTxn()
	txt := frag.InsertXmlText(txn, 0)
	_ = txt.Insert(txn, 0, "Hello world")
	_ = txt.Format(txn, 0, 5, types.Attrs{"strong": true})
	txn.Commit()

	delta := txt.ToDelta()
	if len(delta) != 2 || delta[0].Insert != "Hello" || delta[0].Attributes["strong"] != true {
		t.Errorf("delta = %+v", delta)
	}
}
