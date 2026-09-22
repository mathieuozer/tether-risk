package labels

import (
	"context"
	"os"
	"strings"
	"testing"
)

const ukFixture = `<?xml version="1.0" encoding="utf-8"?>
<Designations>
  <Designation>
    <UniqueID>CTD0004</UniqueID>
    <Names>
      <Name><Name1>Mustafa</Name1><Name6>AYYASH</Name6><NameType>Alias</NameType></Name>
      <Name><Name1>Mustafa</Name1><Name6>AYASH</Name6><NameType>Primary Name</NameType></Name>
    </Names>
    <RegimeName>The Counter-Terrorism (Sanctions) (EU Exit) Regulations 2019</RegimeName>
    <OtherInformation>(1) ETH: 0x175d44451403Edf28469dF03A9280c1197ADb92c (3) USDT:  TGJVc32ig2u8tQsYMLE7KXHT5NDQroaVNU (9) USDT: TGJVc32ig2u8tQsYMLE7KXHT5NDQroaVNV</OtherInformation>
  </Designation>
  <Designation>
    <UniqueID>RUS0001</UniqueID>
    <Names><Name><Name6>NO WALLETS LTD</Name6><NameType>Primary Name</NameType></Name></Names>
    <RegimeName>The Russia (Sanctions) (EU Exit) Regulations 2019</RegimeName>
    <OtherInformation>Tagline TTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTTT is not an address.</OtherInformation>
  </Designation>
</Designations>`

const euFixture = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<export xmlns="http://eu.europa.ec/fpi/fsd/export">
  <sanctionEntity euReferenceNumber="EU.12747.39" logicalId="172907">
    <regulation programme="UKR"/>
    <nameAlias wholeName="Garantex" nameLanguage="EN">
      <remark>Known Garantex blockchain wallet addresses:
TRX: TA1hsikRfsgGiW9nEBpT4tEXEySTNYLr2d</remark>
    </nameAlias>
    <nameAlias wholeName="Гарантекс" nameLanguage="RU"/>
  </sanctionEntity>
</export>`

func TestFreeTextSanctionsUK(t *testing.T) {
	res, ls, err := ParseFreeTextSanctions(context.Background(), strings.NewReader(ukFixture), FormatUK, "uk", 1)
	if err != nil {
		t.Fatal(err)
	}
	if res.Designations != 2 || res.WithAddress != 1 {
		t.Fatalf("designations %d with address %d, want 2 and 1", res.Designations, res.WithAddress)
	}
	var tronN, evmN int
	for _, l := range ls {
		if l.Entity != "Mustafa AYASH" || l.Category != "terrorist_financing" {
			t.Errorf("label %+v: want the primary name and terrorist financing", l)
		}
		switch l.Chain {
		case "tron":
			tronN++
			if l.Address != "TGJVc32ig2u8tQsYMLE7KXHT5NDQroaVNU" {
				t.Errorf("tron address %s", l.Address)
			}
		case "ethereum", "bsc":
			evmN++
			if l.Address != "0x175d44451403edf28469df03a9280c1197adb92c" {
				t.Errorf("evm address %s not lower-cased", l.Address)
			}
		}
	}
	// The address one character off fails its checksum and is dropped.
	if tronN != 1 || evmN != 2 {
		t.Fatalf("tron %d evm %d, want 1 and 2", tronN, evmN)
	}
}

func TestFreeTextSanctionsEU(t *testing.T) {
	_, ls, err := ParseFreeTextSanctions(context.Background(), strings.NewReader(euFixture), FormatEU, "eu", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 1 || ls[0].Address != "TA1hsikRfsgGiW9nEBpT4tEXEySTNYLr2d" || ls[0].Entity != "Garantex" || ls[0].Category != "sanctions" {
		t.Fatalf("got %+v, want Garantex's TRON address as sanctions", ls)
	}
}

// TestFreeTextSanctionsLiveFiles parses downloaded lists when
// SANCTIONS_UK_XML and SANCTIONS_EU_XML point at them.
func TestFreeTextSanctionsLiveFiles(t *testing.T) {
	for _, c := range []struct {
		env    string
		format FreeTextFormat
	}{{"SANCTIONS_UK_XML", FormatUK}, {"SANCTIONS_EU_XML", FormatEU}} {
		p := os.Getenv(c.env)
		if p == "" {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			t.Fatal(err)
		}
		res, ls, err := ParseFreeTextSanctions(context.Background(), f, c.format, string(c.format), 1)
		f.Close()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: %d designations, %d with addresses, by chain %v", c.format, res.Designations, res.WithAddress, res.ByChain)
		for _, l := range ls {
			if l.Chain == "tron" {
				t.Logf("  %s  %s  %s", l.Address, l.Category, l.Entity)
			}
		}
	}
}
