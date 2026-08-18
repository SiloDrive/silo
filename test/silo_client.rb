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

  # --- Repos ---

  def list_repos
    get("/api/silo/v1/repos")
  end

  def create_repo(name)
    post("/api/silo/v1/repos", { name: name })
  end

  def delete_repo(repo_id)
    request(:delete, "/api/silo/v1/repos/#{repo_id}")
  end

  # --- Tokens ---

  def create_access_token(repo_id:, op:, obj_id: "", one_time: false)
    post("/api/silo/v1/access-tokens", {
      repo_id: repo_id, obj_id: obj_id, op: op, one_time: one_time
    })
  end

  def create_sync_token(repo_id)
    post("/api/silo/v1/repos/#{repo_id}/sync-token")
  end

  # --- Sync protocol ---

  def get_head_commit(repo_id, sync_token)
    get("/repo/#{repo_id}/commit/HEAD", sync_token: sync_token)
  end

  # --- Low-level HTTP ---

  def get(path, sync_token: nil)
    request(:get, path, sync_token: sync_token)
  end

  def post(path, body = nil, auth: true)
    request(:post, path, body: body, auth: auth)
  end

  def request(method, path, body: nil, auth: true, sync_token: nil)
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

    if body
      req["Content-Type"] = "application/json"
      req.body = JSON.generate(body)
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
