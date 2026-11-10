# NWS Forecast Chart for TRMNL

A TRMNL private plugin that charts the week's National Weather Service forecast for one or more places, stacked
one above the other. For each location you get:

- the current temperature (measured where possible, see below) and conditions
- each day's forecast high and low, with an icon
- an hourly temperature line for the week, a dashed freezing line, and a marker at the current time
- 6-hour bars for snow (dark) and rain (light), with each storm's total centered under it. A storm is a run of
  blocks with at least 0.02″ of rain or 0.2″ of snow; storms under 0.10″ of rain and 1″ of snow aren't labeled.
  The scale is fixed so it can be learned: the tallest bar, three quarters of the chart's height, is 6″ of snow or
  0.6″ of rain in 6 hours (15 cm / 15 mm); heights are proportional, and anything heavier stops at the tallest bar,
  with the storm's label giving the real amount
- wind, only when gusts reach 25 mph: a dotted curve hanging from the top of the chart for each windy spell, deeper
  for stronger gusts (the bottom of its band is 60 mph), with the spell's peak gust in it
- with station data (see below), the chart starts yesterday: the shaded past shows measured temperatures and
  wind (and measured snow and rain where a source has them), so you can tell what actually happened from what is
  forecast
- at places with snow-depth sensors, the new snow measured in the last 24 and 48 hours (e.g. "❄ 4″ 24h · 9″
  48h"), the number that says whether the skiing is good

Defaults to Leavenworth, WA and Stevens Pass, WA. Shown on a TRMNL X (1872×1404, 16 grays):

![TRMNL X, full screen](docs/x-full.png)

On the original TRMNL (800×480, 1-bit), in full, half horizontal, half vertical and quadrant layouts:

![TRMNL OG layouts](docs/og-layouts.png)

The earlier single-location "Weather Glance" design (day pills shaded by severity) is saved under the git tag
`glance-v1`:

![Weather Glance](docs/glance-full.png)

## How it works

```
TRMNL ──poll──▶ Go service (Vercel or AWS Lambda) ──▶ api.weather.gov (+ forecast.weather.gov)
                  for each location, in parallel:
                  /points → /forecast + /forecast/hourly + /gridpoints + /alerts/active + observations
                  returns ~4 KB of JSON per location, shaped for the templates
```

The NWS data is far over TRMNL's 100 KB polling limit: about 160 KB for the hourly forecast and 250 KB for the
gridpoint data, per location. So a small Go service (`server/`) fetches everything and boils it down to what the
Liquid needs. The temperature line arrives as a ready-made SVG path, and the bars as heights in percent. The TRMNL
side just loops over `locations` and draws.

The chart's hourly temperatures, snowfall and precipitation come from the gridpoint data (`temperature`,
`snowfallAmount`, `quantitativePrecipitation`). The daily highs and lows come from the 12-hour forecast, so they
match the NWS forecast text.

The current temperature is a measured observation, not the forecast for this hour. It is taken from the first
source with a reading less than 2 hours old:

1. The nearest station within 10 miles in api.weather.gov's list for the state: for example LVN11 in Leavenworth
   and STS40 (Schmidt Haus, 3,950 ft) at Stevens Pass. api.weather.gov carries these local networks, including
   NWAC's Stevens Pass stations, with temperatures and (where measured) wind gusts. That station's readings also
   draw yesterday and the part of today that has already happened, and widen today's low/high to include them.
   NWS forecasts start when they're issued, so without this the morning is often blank. (The forecast point's own
   station list isn't used for this: for Stevens Pass it starts at Skykomish.)
2. forecast.weather.gov's point-forecast JSON (`MapClick.php?FcstType=json`), the same feed behind the
   "Current conditions" box on the NWS web page. This endpoint is undocumented.
3. The nearest station in the forecast point's api.weather.gov list with a fresh reading (e.g. KEAT, Wenatchee
   airport).
4. The hourly forecast for the current hour.

A station whose elevation differs from the forecast point's by more than 300 m (about 1,000 ft) is skipped. Without
this, Stevens Pass would show Skykomish's valley reading (SKKW1, 2,028 ft) instead of conditions at about 4,900 ft.

Measured wind gusts are the median of each nearby station's peak in each 6-hour block. api.weather.gov has no
precipitation or snow depth for these stations, so for now the past blocks show the forecast's rain and snow, and
the fresh-snow line is off. When a source with gauges and depth sensors is added (NWAC's Stevens Pass stations),
this is how they're used: measured snow and rain come from all the stations near the location's elevation.
Snowfall isn't measured directly
(the stations' "new snow" readings are unusable), so new snow is the rise in snow depth: each station's depth is
cleaned of spikes, impossible values and sensor jumps and smoothed over 3 hours; rises only count while the station
is at 35°F or colder (sun warms depth sensors too); stations whose depth swings more than an inch hour to hour are
ignored; and the largest rise counts only when a second station rose at least as much as the minimum (0.5″ per bar,
1″ for the 24/48-hour totals). Rain is the median of the precipitation gauges that recorded anything at all, since
a dead gauge reads a steady zero. See `server/forecast/observed.go`.

The payload's `now.source` field says which one was used (`LVN11`, `STS40`, `KEAT`, … or `forecast`), and `now.observed_at`
gives the reading's local time.

## Layout

```
src/                 TRMNL plugin (trmnlp format)
  settings.yml       polling URL + form fields
  shared.liquid      styles, icons, and the location chart / title bar templates
  full.liquid …      one thin view per size
server/              Go service (module root)
  forecast/          everything that matters: NWS client, payload, chart, request handling
  api/index.go       Vercel function: serves forecast.Handler
  vercel.json        Vercel config (serves the function at / and /api)
  main.go            AWS Lambda (Function URL) handler, or a plain HTTP server for local dev
  template.yaml      AWS SAM template
.trmnlp.yml          local preview config
```

## Deploy the service

The service is the same code on either platform. Vercel is the easier one.

### Vercel (recommended)

1. At [vercel.com/new](https://vercel.com/new), import this GitHub repository. The free Hobby plan is enough.
2. Under **Root Directory**, choose `server`. Leave the framework preset as "Other" and the build settings empty.
3. Under **Environment Variables**, add:
   - `ACCESS_TOKEN`: a random string, e.g. from `openssl rand -hex 16`. Requests must include `?token=` with it.
   - `NWS_USER_AGENT`: something with your contact, e.g. `trmnl-nws-weather (you@example.com)`. NWS asks API
     clients to identify themselves.
4. Click **Deploy**. Your service URL is the production domain, e.g. `https://trmnl-nws-weather.vercel.app/`.
   Every push to `main` redeploys it.

If the project has Deployment Protection (Vercel Authentication) turned on, TRMNL will get Vercel's login page
instead of JSON. Turn it off for production under Settings → Deployment Protection; the `ACCESS_TOKEN` already
protects the service.

### AWS Lambda

With the [SAM CLI](https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/install-sam-cli.html):

```sh
cd server
sam build
sam deploy --guided   # set AccessToken to a random string; set NwsUserAgent to include your contact
```

The `ServiceUrl` output is your Function URL. It runs on `provided.al2023` arm64 with 128 MB and costs effectively
nothing at a 30-minute refresh. `server/deploy-policy.json` is a least-privilege IAM policy for the deploying user.

Without SAM, run `make zip` and upload `server/dist/glance.zip` to a new Lambda function: runtime "Amazon Linux 2023",
arm64, handler `bootstrap`. Then enable a Function URL with auth type NONE, and optionally set the `ACCESS_TOKEN` and
`NWS_USER_AGENT` environment variables.

### Try it

```sh
curl -G "https://<your service URL>/" --data-urlencode "token=<ACCESS_TOKEN>" \
  --data-urlencode "locations=Leavenworth, WA | 47.5962,-120.6615;Stevens Pass, WA | 47.7448,-121.0890"
```

Query parameters:
- `locations`: one to four entries separated by newlines or `;`, each `Name | lat,lon` (the name is optional).
  Returns `{"locations": [...], "count": n}`. A location that fails comes back as `{"location", "error"}`, and the
  others still render.
- `lat_lon` and `name`: a single location instead. Returns one object; this is what `glance-v1` uses.
- `units` (`us` or `si`), `bars` (`auto`, `snow` or `precip`), `days` (1–7), and `token`.

## Install the plugin on TRMNL

With [trmnlp](https://github.com/usetrmnl/trmnlp) (`gem install trmnl_preview`):

```sh
trmnlp login
trmnlp push
```

Or create a Private Plugin by hand: strategy **Polling**, paste the `polling_url` and `custom_fields` from
`src/settings.yml`, and copy each `src/*.liquid` file into the matching markup tab (`shared.liquid` → Shared).

Then fill in the plugin settings:
- **Service URL**: your Vercel domain or Lambda Function URL from above.
- **Access token**: your `ACCESS_TOKEN`, if you set one.
- **Locations**: one per line, as `Name | latitude,longitude`. Two fit best on a full screen. The half and quadrant
  layouts show the first location, except half vertical, which stacks all of them compactly.
- **Temperature unit**: Fahrenheit or Celsius.
- **Chart bars**: Auto (snow when any is forecast, otherwise precipitation), Snowfall, or Precipitation.

## Develop locally

```sh
cd server && go test ./... && go run .   # serves on :8080
trmnlp serve                             # previews at http://localhost:4567 using .trmnlp.yml
trmnlp lint
```

`trmnlp` warns that TRMNL itself can't poll `localhost`. That's expected for local preview only. Set
`GLANCE_SERVICE_URL` to preview against your deployed Lambda instead. If Liquid reports "Invalid template encoding",
run with `LANG=C.UTF-8`.
