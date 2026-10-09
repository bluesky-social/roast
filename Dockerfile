# syntax=docker/dockerfile:1
FROM golang:1.27.2 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/roast-broker ./cmd/roast-broker
RUN bash scripts/licenses.sh "linux/$(go env GOARCH)"

FROM gcr.io/distroless/static-debian13:nonroot
COPY --from=build /out/roast-broker /roast-broker
COPY --from=build /src/LICENSE.txt /src/LICENSE-APACHE.txt /src/LICENSE-MIT.txt /usr/share/licenses/roast/
COPY --from=build /src/THIRD_PARTY_LICENSES /usr/share/licenses/roast/third-party/
EXPOSE 8080
ENTRYPOINT ["/roast-broker"]
