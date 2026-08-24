require "net/http"
require "json"
require "uri"

# Lightweight client for Silo's management API (/api/silo/v1), plus the one
# sync-protocol call the token tests need. Used by the test harness — not a
# general-purpose SDK.
class SiloClient
  attr_reader :base_url, :token

  def initialize(base_url)
    @base_url = base_url.chomp("/")
    @token = nil
  end

  # --- Auth ---

  def login(email, password)
    resp = post("/api/silo/v1/auth/login", { email: email, password: password }, auth: false)
    @token = resp["token"]
    resp
  end

  # --- Libraries ---

  def list_libraries
    get("/api/silo/v1/libraries")
  end

  def create_library(name)
    post("/api/silo/v1/libraries", { name: name })
  end

  def delete_library(library_id)
    request(:delete, "/api/silo/v1/libraries/#{library_id}")
  end

  # --- Tokens ---

  def create_access_token(library_id:, op:, obj_id: "", one_time: false)
    post("/api/silo/v1/access-tokens", {
      library_id: library_id, obj_id: obj_id, op: op, one_time: one_time
    })
  end

  def create_sync_token(library_id)
    post("/api/silo/v1/libraries/#{library_id}/sync-token")
  end

  # --- Entries ---

  # The root is /entries/ with nothing after it, not /entries — the route
  # matches the path segment by segment and an absent one is not an empty one.
  def entries_url(library_id, path)
    encoded = path.split("/").map { |seg| URI.encode_www_form_component(seg).gsub("+", "%20") }.join("/")
    encoded = "/" if encoded.empty?
    "/api/silo/v1/libraries/#{library_id}/entries#{encoded}"
  end

  def list_dir(library_id, path = "/", query = nil)
    get("#{entries_url(library_id, path)}#{query ? "?#{query}" : ""}")
  end

  def mkdir(library_id, path)
    request(:put, "#{entries_url(library_id, path)}?type=dir")
  end

  def put_file(library_id, path, content)
    request(:put, entries_url(library_id, path), raw_body: content)
  end

  # --- Blocks ---

  def missing_blocks(library_id, blocks)
    post("/api/silo/v1/libraries/#{library_id}/blocks/missing", { blocks: blocks })
  end

  def put_block(library_id, block_id, content)
    request(:put, "/api/silo/v1/libraries/#{library_id}/blocks/#{block_id}", raw_body: content)
  end

  def create_from_blocks(library_id, path, blocks)
    request(:put, "#{entries_url(library_id, path)}?type=blocks", body: { blocks: blocks })
  end

  # --- Batch ---

  def batch(library_id, ops, if_match: nil)
    request(:post, "/api/silo/v1/libraries/#{library_id}/batch", body: { ops: ops }, if_match: if_match)
  end

  # --- Changes ---

  def changes(library_id, since, query = nil)
    get("/api/silo/v1/libraries/#{library_id}/changes?since=#{since}#{query ? "&#{query}" : ""}")
  end

  # --- Server ---

  def server_info
    get("/api/silo/v1/server-info")
  end

  # --- Sync protocol ---

  def get_head_commit(library_id, sync_token)
    get("/repo/#{library_id}/commit/HEAD", sync_token: sync_token)
  end

  # --- Low-level HTTP ---

  def get(path, sync_token: nil)
    request(:get, path, sync_token: sync_token)
  end

  def post(path, body = nil, auth: true)
    request(:post, path, body: body, auth: auth)
  end

  def request(method, path, body: nil, raw_body: nil, auth: true, sync_token: nil, if_match: nil)
    uri = URI("#{@base_url}#{path}")
    http = Net::HTTP.new(uri.host, uri.port)
    http.open_timeout = 5
    http.read_timeout = 10

    req = case method
          when :get    then Net::HTTP::Get.new(uri)
          when :post   then Net::HTTP::Post.new(uri)
          when :put    then Net::HTTP::Put.new(uri)
          when :delete then Net::HTTP::Delete.new(uri)
          end

    if auth && @token
      req["Authorization"] = "Bearer #{@token}"
    end

    # Wire-protocol header name, fixed by client compatibility — not renamed.
    if sync_token
      req["Seafile-Repo-Token"] = sync_token
    end

    req["If-Match"] = if_match if if_match

    if body
      req["Content-Type"] = "application/json"
      req.body = JSON.generate(body)
    elsif raw_body
      req["Content-Type"] = "application/octet-stream"
      req.body = raw_body
    end

    response = http.request(req)
    Response.new(response)
  end

  # Wraps Net::HTTP response with convenience methods.
  class Response
    attr_reader :http_response

    def initialize(http_response)
      @http_response = http_response
    end

    def status
      @http_response.code.to_i
    end

    def ok?
      status >= 200 && status < 300
    end

    def body
      @http_response.body
    end

    def header(name)
      @http_response[name]
    end

    def json
      @json ||= JSON.parse(body)
    rescue JSON::ParserError
      nil
    end

    def [](key)
      json&.[](key)
    end

    def to_s
      "HTTP #{status}: #{body}"
    end
  end
end
