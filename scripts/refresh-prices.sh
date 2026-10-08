#!/usr/bin/env bash
# Refresh the static price table from the AWS Price List API.
#
# Answers the standing objection to a hand-maintained table: it goes stale. The
# table exists as truffle fallback for when the Price List API is unavailable
# (truffle#175), so the only honest way to keep it is to re-derive it from the
# same API it stands in for.
#
# Prints region<TAB>type<TAB>price on stdout. Diff against pricing/ec2.go, or
# regenerate. Then run: go test ./pricing/ -run Consistency
#
# Fetch real On-Demand Linux/shared prices for Graviton families from the AWS
# Price List API. Output: region<TAB>instanceType<TAB>pricePerHour
set -uo pipefail
REGIONS="us-east-1 us-east-2 us-west-1 us-west-2 eu-west-1 eu-central-1 ap-southeast-1 ap-northeast-1"
FAMS="c6g c7g c8g c9g m6g m7g m8g r6g r7g r8g r9g"
SIZES="large xlarge 2xlarge 4xlarge 8xlarge"

fetch() {
  local region="$1" typ="$2"
  local out
  out=$(aws pricing get-products --region us-east-1 --service-code AmazonEC2 \
    --filters "Type=TERM_MATCH,Field=instanceType,Value=$typ" \
              "Type=TERM_MATCH,Field=regionCode,Value=$region" \
              "Type=TERM_MATCH,Field=operatingSystem,Value=Linux" \
              "Type=TERM_MATCH,Field=tenancy,Value=Shared" \
              "Type=TERM_MATCH,Field=preInstalledSw,Value=NA" \
              "Type=TERM_MATCH,Field=capacitystatus,Value=Used" \
    --query 'PriceList[0]' --output text 2>/dev/null)
  [ -z "$out" ] || [ "$out" = "None" ] && return 0
  printf '%s\n' "$out" | python3 -c "
import json,sys
try:
    d=json.loads(sys.stdin.read())
except Exception:
    sys.exit(0)
for v in d.get('terms',{}).get('OnDemand',{}).values():
    for p in v.get('priceDimensions',{}).values():
        usd=p.get('pricePerUnit',{}).get('USD')
        if usd and float(usd) > 0:
            print('$region\t$typ\t%s' % usd)
        sys.exit(0)
"
}

for region in $REGIONS; do
  for fam in $FAMS; do
    for size in $SIZES; do
      fetch "$region" "$fam.$size" &
      while [ "$(jobs -rp | wc -l)" -ge 4 ]; do wait -n 2>/dev/null || sleep 0.1; done
    done
  done
done
wait
