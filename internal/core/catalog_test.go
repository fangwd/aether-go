package core_test

import (
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"aether/internal/apitest"
)

func TestAPartnerTurnsCommerceOn(t *testing.T) {
	api := apitest.New(t)
	site := uuid.Must(uuid.NewV7())
	uri := "/v1/sites/" + site.String()
	settings := apitest.M{"name": "Cafe", "currency": "AUD", "timezone": "Australia/Sydney"}

	if status, _ := api.Get(uri, ""); status != http.StatusNotFound {
		t.Errorf("before: %d", status)
	}
	if status, body := api.Put(uri, apitest.PartnerKey, settings); status != http.StatusOK {
		t.Errorf("partner: %d %s", status, body)
	}

	status, body := api.Get(uri, "")
	api.Want(status, http.StatusOK, body)
	if body.Get("currency").Str() != "AUD" || body.Get("timezone").Str() != "Australia/Sydney" {
		t.Errorf("site %s", body)
	}
}

func TestSiteSettingsAreValidated(t *testing.T) {
	api := apitest.New(t)
	site := uuid.Must(uuid.NewV7())
	uri := "/v1/sites/" + site.String()

	for _, body := range []apitest.M{
		{"name": "Cafe", "currency": "usd"},
		{"name": "Cafe", "currency": "USD", "timezone": "Mars/Olympus"},
		{"name": " ", "currency": "USD"},
	} {
		if status, problem := api.Put(uri, apitest.PartnerKey, body); status != http.StatusUnprocessableEntity {
			t.Errorf("%v -> %d %s", body, status, problem)
		}
	}
}

func TestAdminRequiresAPartnerKeyAndAnActor(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	products := shop.Admin("/products")

	if status, _ := api.Get(products, ""); status != http.StatusUnauthorized {
		t.Errorf("no key: %d", status)
	}
	if status, _ := api.Get(products, "not-a-partner-key"); status != http.StatusUnauthorized {
		t.Errorf("unknown key: %d", status)
	}
	shopper := shop.Shopper("a@b.co")
	if status, _ := api.Get(products, shopper); status != http.StatusUnauthorized {
		t.Errorf("a shopper session is not a partner key: %d", status)
	}
	if status, body := api.Get(products, shop.Key); status != http.StatusOK {
		t.Errorf("partner: %d %s", status, body)
	}
	req := api.Request(http.MethodGet, products, nil, "Authorization", "Bearer "+shop.Key)
	req.Header.Del("Aether-Actor")
	if status, _ := api.Send(req); status != http.StatusBadRequest {
		t.Errorf("no actor: %d", status)
	}
}

func TestProductWithVariantsAndAddOns(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()

	product := shop.PadThai()

	if product.Get("price").Str() != "15.00" {
		t.Errorf("price %s", product.Get("price"))
	}
	var prices []string
	for _, v := range product.Get("variants").Arr() {
		prices = append(prices, v.Get("price").Str())
	}
	if !reflect.DeepEqual(prices, []string{"15.00", "19.00"}) {
		t.Errorf("variant prices %v", prices)
	}
	if got := product.Get("option_groups", 0, "values", 1, "price_extra").Str(); got != "2.00" {
		t.Errorf("spicy price_extra %q", got)
	}
	if got := product.Get("option_groups", 1, "max_select").Num(); got != 2 {
		t.Errorf("extras max_select %v", got)
	}
}

func TestSimpleProductGetsOneDefaultVariant(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()

	status, product := api.Post(shop.Admin("/products"), shop.Key, apitest.M{"slug": "tee", "name": "Tee", "price": "20"})

	api.Want(status, http.StatusCreated, product)
	if product.Get("status").Str() != "draft" {
		t.Errorf("status %s", product.Get("status"))
	}
	if product.Get("variants").Len() != 1 {
		t.Fatalf("variants %s", product.Get("variants"))
	}
	if !reflect.DeepEqual(product.Get("variants", 0, "attributes").V, map[string]any{}) {
		t.Errorf("attributes %s", product.Get("variants", 0, "attributes"))
	}
	if product.Get("variants", 0, "price").Str() != "20.00" {
		t.Errorf("variant price %s", product.Get("variants", 0, "price"))
	}
}

func TestInvalidProductsAreRejected(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	uri := shop.Admin("/products")
	shop.PadThai()

	cases := []struct {
		body apitest.M
		want int
	}{
		{apitest.M{"slug": "pad-thai", "name": "Again", "price": "1"}, http.StatusConflict},
		{apitest.M{"slug": "x", "name": "X", "price": "1.005"}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X", "price": "10", "compare_at_price": "9"}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "Bad Slug", "name": "X", "price": "1"}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X", "price": "-1"}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X", "price": "1", "variant_axes": []string{"Size"},
			"variants": []apitest.M{{"attributes": apitest.M{"Colour": "Red"}}}}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X", "price": "1", "variant_axes": []string{"Size"},
			"variants": []apitest.M{{"attributes": apitest.M{"Size": "S"}}, {"attributes": apitest.M{"Size": "S"}}}}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X", "price": "1", "variants": []apitest.M{{"price_extra": "-2"}}}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X", "price": "1", "option_groups": []apitest.M{
			{"code": "g", "name": "G", "min_select": 2, "max_select": 1, "values": []apitest.M{{"code": "a", "label": "A"}}},
		}}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X"}, http.StatusUnprocessableEntity},
		{apitest.M{"slug": "x", "name": "X", "price": "1", "category_ids": []string{uuid.Must(uuid.NewV7()).String()}}, http.StatusUnprocessableEntity},
	}
	for _, c := range cases {
		if status, problem := api.Post(uri, shop.Key, c.body); status != c.want {
			t.Errorf("%v -> %d, want %d: %s", c.body, status, c.want, problem)
		}
	}
}

func TestStorefrontShowsActiveProductsOnly(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	shop.PadThai()
	api.Post(shop.Admin("/products"), shop.Key, apitest.M{"slug": "draft", "name": "Draft", "price": "1", "cost_amount": "0.40"})

	status, page := api.Get(shop.Store("/products"), "")
	api.Want(status, http.StatusOK, page)
	var slugs []string
	for _, p := range page.Get("items").Arr() {
		slugs = append(slugs, p.Get("slug").Str())
	}
	if !reflect.DeepEqual(slugs, []string{"pad-thai"}) {
		t.Errorf("slugs %v", slugs)
	}

	if status, _ := api.Get(shop.Store("/products/draft"), ""); status != http.StatusNotFound {
		t.Errorf("draft on the storefront: %d", status)
	}
	status, product := api.Get(shop.Store("/products/pad-thai"), "")
	api.Want(status, http.StatusOK, product)
	if _, ok := product.V.(map[string]any)["cost_amount"]; ok {
		t.Errorf("the storefront shows cost_amount: %s", product)
	}
	if product.Get("variants", 0, "available").V != true {
		t.Errorf("available %s", product.Get("variants", 0))
	}
}

func TestPagesFollowTheCursor(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	for i := range 3 {
		api.Post(shop.Admin("/products"), shop.Key, apitest.M{"slug": fmt.Sprintf("p%d", i), "name": "P", "price": "1"})
	}

	_, first := api.Get(shop.Admin("/products")+"?limit=2", shop.Key)
	cursor := first.Get("next_cursor").Str()
	if cursor == "" {
		t.Fatalf("first page %s", first)
	}
	_, second := api.Get(shop.Admin("/products")+"?limit=2&cursor="+cursor, shop.Key)

	if first.Get("items").Len() != 2 {
		t.Errorf("first page %d items", first.Get("items").Len())
	}
	if second.Get("items").Len() != 1 {
		t.Errorf("second page %d items", second.Get("items").Len())
	}
	if second.Get("next_cursor").V != nil {
		t.Errorf("next_cursor %s", second.Get("next_cursor"))
	}
}

func TestPatchUpdatesFieldsAndNullClears(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	product := shop.PadThai()
	uri := shop.Admin("/products/" + product.Get("id").Str())

	status, updated := api.Patch(uri, shop.Key, apitest.M{"price": "16", "compare_at_price": "18", "description": "Noodles"})
	api.Want(status, http.StatusOK, updated)
	if got := updated.Get("variants", 1, "price").Str(); got != "20.00" {
		t.Errorf("large price %q", got)
	}

	_, cleared := api.Patch(uri, shop.Key, apitest.M{"compare_at_price": nil})
	if cleared.Get("compare_at_price").V != nil {
		t.Errorf("compare_at_price %s", cleared.Get("compare_at_price"))
	}
	if cleared.Get("description").Str() != "Noodles" {
		t.Errorf("description %s", cleared.Get("description"))
	}

	if status, _ := api.Patch(uri, shop.Key, apitest.M{"price": "1", "compare_at_price": "0.5"}); status != http.StatusUnprocessableEntity {
		t.Errorf("compare_at below price: %d", status)
	}
}

func TestReplacingVariantsKeepsIdentityAndHistory(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	product := shop.PadThai()
	productID := product.Get("id").Str()
	regular := apitest.VariantID(product, "Regular")
	large := apitest.VariantID(product, "Large")
	// Stock history on Large means it must survive removal, deactivated.
	api.Post(shop.Admin("/inventory/"+large+"/adjustments"), shop.Key, apitest.M{"quantity": 3, "kind": "receipt"})

	status, updated := api.Put(shop.Admin("/products/"+productID+"/variants"), shop.Key, apitest.M{
		"variant_axes": []string{"Size"},
		"variants": []apitest.M{
			// SKUs swap between kept rows; checked only at commit.
			{"id": regular, "sku": "PT-L", "attributes": apitest.M{"Size": "Regular"}},
			{"sku": "PT-R", "attributes": apitest.M{"Size": "Small"}, "price_extra": "-2.00"},
		},
	})
	api.Want(status, http.StatusOK, updated)

	find := func(variants apitest.J, size string) apitest.J {
		for _, v := range variants.Arr() {
			if v.Get("attributes", "Size").Str() == size {
				return v
			}
		}
		t.Fatalf("no %s variant in %s", size, variants)
		return apitest.J{}
	}
	variants := updated.Get("variants")
	if got := find(variants, "Regular"); got.Get("id").Str() != regular || got.Get("sku").Str() != "PT-L" {
		t.Errorf("Regular %s", got)
	}
	if got := find(variants, "Small").Get("price").Str(); got != "13.00" {
		t.Errorf("Small price %q", got)
	}
	if got := find(variants, "Large"); got.Get("status").Str() != "inactive" || got.Get("sku").V != nil {
		t.Errorf("Large %s", got)
	}

	// Bringing Large back by its axis values revives the same variant.
	_, revived := api.Put(shop.Admin("/products/"+productID+"/variants"), shop.Key, apitest.M{
		"variant_axes": []string{"Size"},
		"variants":     []apitest.M{{"attributes": apitest.M{"Size": "Large"}, "price_extra": "4"}},
	})
	if got := revived.Get("variants").Find("status", "active").Get("id").Str(); got != large {
		t.Errorf("revived %s, want id %s", revived.Get("variants"), large)
	}
}

func TestReplacingOptionGroupsKeepsIdsByCode(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	product := shop.PadThai()
	spicy := apitest.OptionID(product, "spice", "spicy")

	status, updated := api.Put(shop.Admin("/products/"+product.Get("id").Str()+"/option-groups"), shop.Key, apitest.M{
		"option_groups": []apitest.M{{"code": "spice", "name": "Heat", "max_select": 1, "values": []apitest.M{
			{"code": "spicy", "label": "Hot", "price_extra": "2.50"},
		}}},
	})

	api.Want(status, http.StatusOK, updated)
	if updated.Get("option_groups").Len() != 1 {
		t.Errorf("option groups %s", updated.Get("option_groups"))
	}
	if got := apitest.OptionID(updated, "spice", "spicy"); got != spicy {
		t.Errorf("spicy id %q, want %q", got, spicy)
	}
	if got := updated.Get("option_groups", 0, "values", 0, "price_extra").Str(); got != "2.50" {
		t.Errorf("price_extra %q", got)
	}
}

func TestStockDrivesAvailability(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	product := shop.PadThai()
	large := apitest.VariantID(product, "Large")
	adjust := shop.Admin("/inventory/" + large + "/adjustments")

	status, level := api.Post(adjust, shop.Key, apitest.M{"quantity": 5, "kind": "receipt"})
	api.Want(status, http.StatusOK, level)
	if level.Get("available").Num() != 5 {
		t.Errorf("level %s", level)
	}

	api.Post(adjust, shop.Key, apitest.M{"quantity": -5, "kind": "adjustment", "note": "spilled"})
	_, product = api.Get(shop.Store("/products/pad-thai"), "")
	if product.Get("variants", 1, "available").V != false {
		t.Errorf("sold-out Large: %s", product.Get("variants", 1))
	}
	if product.Get("variants", 0, "available").V != true {
		t.Errorf("untracked stock is always available: %s", product.Get("variants", 0))
	}

	if status, _ := api.Post(adjust, shop.Key, apitest.M{"quantity": -1, "kind": "adjustment"}); status != http.StatusUnprocessableEntity {
		t.Errorf("negative stock: %d", status)
	}
	_, levels := api.Get(shop.Admin("/inventory"), shop.Key)
	if levels.Len() != 1 {
		t.Errorf("levels %s", levels)
	}
	// Unnamed, so named by its axis values.
	if got := levels.Get(0, "variant_name").V; got != "Large" {
		t.Errorf("variant_name %v", got)
	}
}

func TestCategoriesFilterTheStorefront(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()
	_, noodles := api.Post(shop.Admin("/categories"), shop.Key, apitest.M{"slug": "noodles", "name": "Noodles"})
	_, thai := api.Post(shop.Admin("/categories"), shop.Key, apitest.M{"slug": "thai", "name": "Thai", "parent_id": noodles.Get("id").V})
	product := shop.PadThai()
	api.Patch(shop.Admin("/products/"+product.Get("id").Str()), shop.Key, apitest.M{"category_ids": []any{thai.Get("id").V}})

	_, inThai := api.Get(shop.Store("/products")+"?category=thai", "")
	_, inNoodles := api.Get(shop.Store("/products")+"?category=noodles", "")
	if inThai.Get("items").Len() != 1 || inThai.Get("items", 0, "categories", 0, "slug").Str() != "thai" {
		t.Errorf("in thai %s", inThai)
	}
	if inNoodles.Get("items").Len() != 0 {
		t.Errorf("in noodles %s", inNoodles)
	}

	status, _ := api.Put(shop.Admin("/categories/"+noodles.Get("id").Str()), shop.Key,
		apitest.M{"slug": "noodles", "name": "Noodles", "parent_id": thai.Get("id").V})
	if status != http.StatusUnprocessableEntity {
		t.Errorf("category cycle: %d", status)
	}
	if _, listed := api.Get(shop.Store("/categories"), ""); listed.Len() != 2 {
		t.Errorf("categories %s", listed)
	}
}

func TestSitesNeverSeeEachOthersCatalog(t *testing.T) {
	api := apitest.New(t)
	mine := api.OpenShop()
	theirs := api.OpenShop()
	product := theirs.PadThai()
	productID := product.Get("id").Str()

	if status, _ := api.Get(mine.Admin("/products/"+productID), mine.Key); status != http.StatusNotFound {
		t.Errorf("their product in my admin: %d", status)
	}
	if status, _ := api.Get(mine.Store("/products/pad-thai"), ""); status != http.StatusNotFound {
		t.Errorf("their product on my storefront: %d", status)
	}
	if status, _ := api.Patch(mine.Admin("/products/x"), mine.Key, apitest.M{}); status != http.StatusBadRequest {
		t.Errorf("malformed product id: %d", status)
	}
}

func TestMediaUploadsGoToTheImageStore(t *testing.T) {
	api := apitest.New(t)
	api.Server.Images = &apitest.FakeImages{}
	api.Rebuild()
	shop := api.OpenShop()

	status, upload := api.Post(shop.Admin("/media"), shop.Key, apitest.M{"mime_type": "image/png", "alt": "Bowl"})
	api.Want(status, http.StatusCreated, upload)
	if upload.Get("upload_url").Str() != "https://upload.example/1" {
		t.Errorf("upload %s", upload)
	}

	_, product := api.Post(shop.Admin("/products"), shop.Key,
		apitest.M{"slug": "bowl", "name": "Bowl", "price": "5", "media": []apitest.M{{"media_id": upload.Get("media_id").V}}})
	if product.Get("media", 0, "url").Str() != "https://images.example/img-1" || product.Get("media", 0, "alt").Str() != "Bowl" {
		t.Errorf("media %s", product.Get("media"))
	}
}

func TestMediaUploadsNeedImageStorage(t *testing.T) {
	api := apitest.New(t)
	shop := api.OpenShop()

	if status, _ := api.Post(shop.Admin("/media"), shop.Key, apitest.M{"mime_type": "image/png"}); status != http.StatusServiceUnavailable {
		t.Errorf("status %d", status)
	}
}
