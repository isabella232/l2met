require 'librato/metrics'
require 'scrolls'
require 'redis'
require 'locksmith/dynamodb'

require 'l2met/db'
require 'l2met/utils'
require 'l2met/config'
require 'l2met/stats'

module L2met
  module Outlet
    extend self
    DELAY = 60

    def start
      loop do
        Config.num_outlets.times.each do |p|
          locker.lock("#{Config.app_name}.outlet.#{p}", ttl: 60) do
            bucket = Utils.trunc_time(Time.now) - DELAY
            snapshot(p, Config.num_outlets, bucket)
          end
        end
      end
    end

    def snapshot(partition, max, bucket)
      Utils.measure('outlet.snapshot') do
        # redis layout: mkey:bucket:uuid
        Utils.measure('outlet.redis.key-scan') do
          redis.keys("*:#{bucket}:*")
        end.select do |key|
          Integer(key.split(':')[0]) % max == partition
        end.map do |key|
          mkey = key.split(':')[0]
          Utils.measure('outlet.redis.get') do
            redis.hgetall(key).merge('mkey' => mkey)
          end
        end.group_by do |metric|
          metric["consumer"]
        end.each do |consumer_id, metrics|
          begin
            q = Librato::Metrics::Queue.new(client: librato_client(consumer_id))
            aggregate(bucket, metrics).each {|m| q.add(m)}
            if q.length > 0
              Utils.count(q.length, 'outlet.librato.metrics')
              Utils.measure('outlet.librato.submit') {q.submit}
            end
          rescue => e
            Utils.count(1, 'outlet.metric-post-error')
            log(fn: __method__, at: 'error', consumer: consumer_id,
              error: e.inspect)
            next
          end
        end
      end
    end

    def aggregate(bucket, metrics)
      metrics.group_by do |metric|
        metric['mkey']
      end.map do |mkey, metrics|
        s = metrics.sample
        opts = {source: s["source"],
          type: "gauge",
          attributes: {display_units_long: s["label"]},
          measure_time: bucket}
        log(fn: __method__, at: "process", bucket: Time.at(bucket).min, metric: s["name"])
        case s["type"]
        when "counter"
          val = metrics.map {|m| Float(m["value"])}.reduce(:+)
          name = [s["name"], 'count'].join(".")
          {name => opts.merge(value: val)}
        when "last"
          val = Float(metrics.last["value"])
          name = [s["name"], 'last'].join(".")
          {name => opts.merge(value: val)}
        when "list"
          Stats.aggregate(metrics).map do |stat, val|
            name = [s["name"], stat].map(&:to_s).join(".")
            {name => opts.merge(value: val)}
          end
        end
      end.flatten.compact
    end

    def librato_client(consumer_id)
      consumer = DB["consumers"].at(consumer_id).attributes
      Librato::Metrics::Client.new.tap do |c|
        c.authenticate(consumer["email"], consumer["token"])
      end
    end

    def redis
      @redis ||= Redis.new(url: Config.redis_url)
    end

    def locker
      @locker ||= Locksmith::Dynamodb
    end

    def log(data, &blk)
      Scrolls.log({ns: "outlet"}.merge(data), &blk)
    end

  end
end