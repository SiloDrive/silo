require_relative "test_helper"
require "securerandom"

class AuthTest < Minitest::Test
  include SiloTestHelper

  def test_login_returns_jwt
    c = SiloClient.new(silo_url)
    resp = c.login(silo_email, silo_password)
    assert resp.ok?, "Login failed: #{resp}"
    assert resp["token"], "Expected token in response"
    assert resp["token"].include?("."), "Token should be a JWT (contains dots)"
  end

  def test_login_wrong_password
    c = SiloClient.new(silo_url)
    resp = c.post("/api/silo/v1/auth/login", { email: silo_email, password: "wrong" }, auth: false)
    assert_equal 401, resp.status
  end

  def test_login_missing_fields
    c = SiloClient.new(silo_url)

    resp = c.post("/api/silo/v1/auth/login", { email: silo_email }, auth: false)
    assert_equal 400, resp.status

    resp = c.post("/api/silo/v1/auth/login", { password: "whatever" }, auth: false)
    assert_equal 400, resp.status
  end

  def test_login_nonexistent_user
    c = SiloClient.new(silo_url)
    resp = c.post("/api/silo/v1/auth/login", { email: "nobody@example.com", password: "x" }, auth: false)
    assert_equal 401, resp.status
  end

  def test_protected_endpoints_reject_no_auth
    c = SiloClient.new(silo_url)

    resp = c.request(:get, "/api/silo/v1/repos", auth: false)
    assert_equal 401, resp.status

    resp = c.request(:post, "/api/silo/v1/repos", body: { name: "x" }, auth: false)
    assert_equal 401, resp.status
  end

  def test_protected_endpoints_reject_bad_token
    c = SiloClient.new(silo_url)
    c.instance_variable_set(:@token, "not.a.valid.jwt.token")

    resp = c.list_repos
    assert_equal 401, resp.status
  end
end
